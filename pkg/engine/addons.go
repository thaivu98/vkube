package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

// runAddons cài đặt các Core Addons được bật (metrics-server, local-path, MetalLB, Kong Ingress)
// và các customManifests do người dùng khai báo trong cluster.yaml (100% offline từ Offline Bundle).
func (e *Engine) runAddons(ctx context.Context, state *ClusterState) error {
	addons := state.Config.Spec.Addons
	hasAny := addons.MetricsServer.Enabled ||
		addons.LocalPathStorage.Enabled ||
		addons.MetalLB.Enabled ||
		addons.KongIngress.Enabled ||
		len(addons.CustomManifests) > 0

	if !hasAny {
		return nil
	}

	cp1 := state.Config.ControlPlanes()[0]
	cp1Runner, err := state.Pool.Get(cp1.Name)
	if err != nil {
		return err
	}

	fmt.Fprintf(e.Out, "==> [Addons] Triển khai các Addons đã bật trong cluster.yaml...\n")

	// 1. Metrics Server (HPA & kubectl top) — tự thêm --kubelet-insecure-tls cho kubeadm on-prem
	if addons.MetricsServer.Enabled {
		fmt.Fprintf(e.Out, "  [addon]     metrics-server (HPA / kubectl top)...\n")
		script := fmt.Sprintf(`set -euo pipefail
F="%s/metrics-server.yaml"
sed -i 's/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g' "$F"
if ! grep -q -- '--kubelet-insecure-tls' "$F"; then
  sed -i '/--kubelet-preferred-address-types=/a\        - --kubelet-insecure-tls' "$F"
fi
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$F"
`, remoteManifestDir)
		if _, err := cp1Runner.RunSudo(ctx, script); err != nil {
			return fmt.Errorf("install metrics-server addon: %w", err)
		}
	}

	// 2. Local Path Provisioner (StorageClass mặc định cho PVC động)
	if addons.LocalPathStorage.Enabled {
		fmt.Fprintf(e.Out, "  [addon]     local-path-provisioner (Default StorageClass)...\n")
		script := fmt.Sprintf(`set -euo pipefail
F="%s/local-path-storage.yaml"
sed -i 's/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g' "$F"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$F"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf patch storageclass local-path \
  -p '{"metadata": {"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
`, remoteManifestDir)
		if _, err := cp1Runner.RunSudo(ctx, script); err != nil {
			return fmt.Errorf("install local-path-provisioner addon: %w", err)
		}
	}

	// 3. MetalLB (Bare-metal LoadBalancer L2) + tự tạo IPAddressPool & L2Advertisement
	if addons.MetalLB.Enabled {
		fmt.Fprintf(e.Out, "  [addon]     metallb (L2 LoadBalancer, ipRange=%s)...\n", addons.MetalLB.IPRange)
		poolManifest := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: vkube-default-pool
  namespace: metallb-system
spec:
  addresses:
    - %s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: vkube-default-l2
  namespace: metallb-system
spec:
  ipAddressPools:
    - vkube-default-pool
`, addons.MetalLB.IPRange)

		if err := cp1Runner.WriteFile(ctx, []byte(poolManifest), "/tmp/vkube-metallb-pool.yaml", 0o644); err != nil {
			return err
		}

		script := fmt.Sprintf(`set -euo pipefail
F="%s/metallb-native.yaml"
sed -i 's/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g' "$F"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$F"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf -n metallb-system rollout status deployment/controller --timeout=120s
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f /tmp/vkube-metallb-pool.yaml
rm -f /tmp/vkube-metallb-pool.yaml
`, remoteManifestDir)
		if _, err := cp1Runner.RunSudo(ctx, script); err != nil {
			return fmt.Errorf("install metallb addon: %w", err)
		}
	}

	// 4. Kong Ingress Controller (DB-less mode)
	if addons.KongIngress.Enabled {
		fmt.Fprintf(e.Out, "  [addon]     kong-ingress-controller (DB-less API Gateway)...\n")
		script := fmt.Sprintf(`set -euo pipefail
F="%s/kong-dbless.yaml"
sed -i 's/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g' "$F"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$F"
`, remoteManifestDir)
		if _, err := cp1Runner.RunSudo(ctx, script); err != nil {
			return fmt.Errorf("install kong ingress addon: %w", err)
		}
	}

	// 5. Custom Manifests do người dùng khai báo thêm trong cluster.yaml
	for i, localPath := range addons.CustomManifests {
		data, err := os.ReadFile(localPath)
		if err != nil {
			return fmt.Errorf("read custom manifest %q: %w", localPath, err)
		}
		base := filepath.Base(localPath)
		remoteTmp := fmt.Sprintf("/tmp/vkube-custom-%d-%s", i, base)
		fmt.Fprintf(e.Out, "  [custom]    applying %s...\n", localPath)
		if err := cp1Runner.WriteFile(ctx, data, remoteTmp, 0o644); err != nil {
			return fmt.Errorf("upload custom manifest %q: %w", localPath, err)
		}
		cmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f %s && rm -f %s", remoteTmp, remoteTmp)
		if _, err := cp1Runner.RunSudo(ctx, cmd); err != nil {
			return fmt.Errorf("apply custom manifest %q: %w", localPath, err)
		}
	}

	return nil
}

// renderRegistryHostsToml sinh nội dung chuẩn /etc/containerd/certs.d/<host>/hosts.toml
// hỗ trợ Private Registry / Harbor nội bộ (bao gồm cả HTTP hoặc HTTPS self-signed CA).
func renderRegistryHostsToml(m api.RegistryMirror, remoteCAPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "server = %q\n\n", "https://"+m.Host)
	fmt.Fprintf(&b, "[host.%q]\n", m.Endpoint)
	b.WriteString("  capabilities = [\"pull\", \"resolve\"]\n")
	if m.Insecure {
		b.WriteString("  skip_verify = true\n")
	}
	if remoteCAPath != "" {
		fmt.Fprintf(&b, "  ca = %q\n", remoteCAPath)
	}
	return b.String()
}

// configureRegistryMirrors cấu hình /etc/containerd/certs.d trên một Target Node.
func configureRegistryMirrors(ctx context.Context, mirrors []api.RegistryMirror, r ssh.Runner) error {
	for i, m := range mirrors {
		var remoteCAPath string
		if m.CACertPath != "" {
			caBytes, err := os.ReadFile(m.CACertPath)
			if err != nil {
				return fmt.Errorf("read registry CA cert %q for %s: %w", m.CACertPath, m.Host, err)
			}
			tmpCA := fmt.Sprintf("/tmp/vkube-reg-ca-%d.crt", i)
			if err := r.WriteFile(ctx, caBytes, tmpCA, 0o644); err != nil {
				return err
			}
			remoteCAPath = fmt.Sprintf("/etc/containerd/certs.d/%s/ca.crt", m.Host)
			if _, err := r.RunSudo(ctx, fmt.Sprintf("mkdir -p /etc/containerd/certs.d/%s && mv -f %s %s", m.Host, tmpCA, remoteCAPath)); err != nil {
				return err
			}
		}

		tomlContent := renderRegistryHostsToml(m, remoteCAPath)
		tmpToml := fmt.Sprintf("/tmp/vkube-reg-hosts-%d.toml", i)
		if err := r.WriteFile(ctx, []byte(tomlContent), tmpToml, 0o644); err != nil {
			return err
		}
		cmd := fmt.Sprintf("mkdir -p /etc/containerd/certs.d/%s && mv -f %s /etc/containerd/certs.d/%s/hosts.toml", m.Host, tmpToml, m.Host)
		if _, err := r.RunSudo(ctx, cmd); err != nil {
			return fmt.Errorf("configure registry mirror %s: %w", m.Host, err)
		}
	}
	return nil
}
