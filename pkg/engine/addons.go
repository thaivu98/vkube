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

		if err := cp1Runner.WriteFile(ctx, []byte(poolManifest), ".vkube-tmp/metallb-pool.yaml", 0o600); err != nil {
			return err
		}

		cp1User := state.Config.EffectiveSSH(cp1).User
		script := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
F="%s/metallb-native.yaml"
sed -i 's/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g' "$F"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$F"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf -n metallb-system rollout status deployment/controller --timeout=120s
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$USER_HOME/.vkube-tmp/metallb-pool.yaml"
rm -f "$USER_HOME/.vkube-tmp/metallb-pool.yaml"
`, cp1User, remoteManifestDir)
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
	cp1User := state.Config.EffectiveSSH(cp1).User
	for i, localPath := range addons.CustomManifests {
		data, err := os.ReadFile(localPath)
		if err != nil {
			return fmt.Errorf("read custom manifest %q: %w", localPath, err)
		}
		base := filepath.Base(localPath)
		relTmp := fmt.Sprintf(".vkube-tmp/custom-%d-%s", i, base)
		fmt.Fprintf(e.Out, "  [custom]    applying %s...\n", localPath)
		if err := cp1Runner.WriteFile(ctx, data, relTmp, 0o600); err != nil {
			return fmt.Errorf("upload custom manifest %q: %w", localPath, err)
		}
		cmd := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$USER_HOME/%s"
rm -f "$USER_HOME/%s"`, cp1User, relTmp, relTmp)
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

// configureRegistryMirrors cấu hình /etc/containerd/certs.d trên một Target Node qua thư mục riêng .vkube-tmp.
func configureRegistryMirrors(ctx context.Context, mirrors []api.RegistryMirror, r ssh.Runner, sshUser string) error {
	for i, m := range mirrors {
		var remoteCAPath string
		if m.CACertPath != "" {
			caBytes, err := os.ReadFile(m.CACertPath)
			if err != nil {
				return fmt.Errorf("read registry CA cert %q for %s: %w", m.CACertPath, m.Host, err)
			}
			relCA := fmt.Sprintf(".vkube-tmp/reg-ca-%d.crt", i)
			if err := r.WriteFile(ctx, caBytes, relCA, 0o600); err != nil {
				return err
			}
			remoteCAPath = fmt.Sprintf("/etc/containerd/certs.d/%s/ca.crt", m.Host)
			cmd := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
mkdir -p /etc/containerd/certs.d/%s
install -m 0644 "$USER_HOME/%s" %s
rm -f "$USER_HOME/%s"`, sshUser, m.Host, relCA, remoteCAPath, relCA)
			if _, err := r.RunSudo(ctx, cmd); err != nil {
				return err
			}
		}

		tomlContent := renderRegistryHostsToml(m, remoteCAPath)
		relToml := fmt.Sprintf(".vkube-tmp/reg-hosts-%d.toml", i)
		if err := r.WriteFile(ctx, []byte(tomlContent), relToml, 0o600); err != nil {
			return err
		}
		cmd := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
mkdir -p /etc/containerd/certs.d/%s
install -m 0644 "$USER_HOME/%s" /etc/containerd/certs.d/%s/hosts.toml
rm -f "$USER_HOME/%s"`, sshUser, m.Host, relToml, m.Host, relToml)
		if _, err := r.RunSudo(ctx, cmd); err != nil {
			return fmt.Errorf("configure registry mirror %s: %w", m.Host, err)
		}
	}
	return nil
}
