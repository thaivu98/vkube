package engine

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/bundle"
	"github.com/thaivu98/vkube/pkg/ssh"
	"github.com/thaivu98/vkube/pkg/templates"
	"github.com/thaivu98/vkube/pkg/versions"
)

// ApplyAddonsOptions chứa tham số cho lệnh `vkube addons apply`.
type ApplyAddonsOptions struct {
	BundlePath   string // Nếu có -> Air-gapped (không gọi curl ra Internet)
	ManifestPath string
}

// ApplyAddons đồng bộ lại Core Addons, customManifests, Labels/Taints, Proxy và Private Registry Mirrors
// lên một cụm Kubernetes đang vận hành mà không cần dựng lại cụm (hỗ trợ cả Online và Air-gapped --bundle).
func (e *Engine) ApplyAddons(ctx context.Context, cfg *api.ClusterConfig, opts ApplyAddonsOptions) error {
	start := time.Now()
	onlineMode := strings.TrimSpace(opts.BundlePath) == ""

	var (
		manifest *api.BundleManifest
		bom      *versions.BOM
		err      error
	)
	if onlineMode {
		bom, err = versions.Load(opts.ManifestPath)
		if err != nil {
			return fmt.Errorf("load BOM manifest: %w", err)
		}
	} else {
		manifest, err = bundle.Verify(opts.BundlePath)
		if err != nil {
			return fmt.Errorf("verify offline bundle: %w", err)
		}
	}

	pool, err := ssh.ConnectAll(ctx, cfg, e.Out)
	if err != nil {
		return err
	}
	defer pool.Close()

	cp1 := cfg.ControlPlanes()[0]
	cp1Runner, err := pool.Get(cp1.Name)
	if err != nil {
		return err
	}

	var resolved *versions.ResolvedBOM
	if onlineMode {
		res, err := cp1Runner.RunSudo(ctx, "uname -m")
		if err != nil {
			return fmt.Errorf("detect arch on %s: %w", cp1.Name, err)
		}
		arch, err := normalizeLinuxArch(res.Stdout)
		if err != nil {
			return err
		}
		resolved, err = bom.Resolve(arch)
		if err != nil {
			return err
		}
		manifest = &api.BundleManifest{
			ReleaseTag:        resolved.ReleaseTag,
			KubernetesVersion: resolved.KubernetesVersion,
			Arch:              resolved.Arch,
			Components:        resolved.Components,
		}

		// Tải bổ sung các manifest addon chưa có trong /etc/vkube/manifests (kèm verify SHA256 ghim trong BOM)
		var fetchCmds []string
		fetchCmds = append(fetchCmds, "set -euo pipefail")
		if pe := proxyShellExports(cfg); pe != "" {
			fetchCmds = append(fetchCmds, pe)
		}
		fetchCmds = append(fetchCmds, fmt.Sprintf("mkdir -p %s", remoteManifestDir))
		for _, art := range resolved.Artifacts {
			if art.Type != versions.ArtifactTypeManifest {
				continue
			}
			destPath := path.Join(remoteManifestDir, path.Base(art.Dest))
			fetchCmds = append(fetchCmds, fmt.Sprintf(`if [ ! -f %q ]; then
  curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 120 %q -o %q
  echo "%s  %s" | sha256sum -c - >/dev/null
fi`, destPath, art.URL, destPath, art.SHA256, destPath))
		}
		if _, err := cp1Runner.RunSudo(ctx, strings.Join(fetchCmds, "\n")); err != nil {
			return fmt.Errorf("ensure addon manifests on %s: %w", cp1.Name, err)
		}
	} else {
		// Chế độ Air-gapped: trích xuất manifests trực tiếp từ Offline Bundle lên CP-1 (0 byte tải từ Internet)
		remoteArchive := "/var/tmp/vkube-addons-bundle.tar.zst"
		if err := cp1Runner.UploadFile(ctx, opts.BundlePath, remoteArchive); err != nil {
			return fmt.Errorf("upload bundle to %s: %w", cp1.Name, err)
		}
		extractCmd := fmt.Sprintf(`set -euo pipefail
ADIR="/var/lib/vkube/addons-staging"
rm -rf "$ADIR" && mkdir -p -m 0700 "$ADIR" %s
trap 'rm -rf "$ADIR" %s' EXIT
tar --zstd -xf %s -C "$ADIR"
if [ -d "$ADIR/manifests" ]; then
  cp -f "$ADIR/manifests/"* %s/
fi`, remoteManifestDir, remoteArchive, remoteArchive, remoteManifestDir)
		if _, err := cp1Runner.RunSudo(ctx, extractCmd); err != nil {
			return fmt.Errorf("extract offline addon manifests on %s: %w", cp1.Name, err)
		}
	}

	state := &ClusterState{
		Config:         cfg,
		OnlineMode:     onlineMode,
		ResolvedBOM:    resolved,
		BundleManifest: manifest,
		Pool:           pool,
		Facts:          make(map[string]*api.NodeFacts),
	}

	if err := e.runAddons(ctx, state); err != nil {
		return err
	}

	// Đồng bộ Labels & Taints trên toàn bộ nodes
	fmt.Fprintf(e.Out, "==> [Reconcile] Đồng bộ Labels/Taints trên %d node(s)...\n", len(cfg.Spec.Nodes))
	for _, node := range cfg.Spec.Nodes {
		for k, v := range node.Labels {
			cmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf label node %s %s=%s --overwrite", node.Name, k, v)
			if _, err := cp1Runner.RunSudo(ctx, cmd); err != nil {
				return fmt.Errorf("apply label %s=%s on %s: %w", k, v, node.Name, err)
			}
		}
		for _, t := range node.Taints {
			taintSpec := fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect)
			if t.Value == "" {
				taintSpec = fmt.Sprintf("%s:%s", t.Key, t.Effect)
			}
			cmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf taint node %s %s --overwrite", node.Name, taintSpec)
			if _, err := cp1Runner.RunSudo(ctx, cmd); err != nil {
				return fmt.Errorf("apply taint %s on %s: %w", taintSpec, node.Name, err)
			}
		}
	}

	// Đồng bộ Private Registry Mirrors & Proxy drop-in trên toàn bộ nodes nếu có khai báo
	if len(cfg.Spec.Registry.Mirrors) > 0 || hasProxyConfig(cfg.Spec.Proxy) {
		fmt.Fprintf(e.Out, "==> [Reconcile] Đồng bộ Registry Mirrors / HTTP Proxy trên %d node(s)...\n", len(cfg.Spec.Nodes))
		if err := pool.RunParallel(ctx, cfg.Spec.Nodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
			u := cfg.EffectiveSSH(node).User
			if len(cfg.Spec.Registry.Mirrors) > 0 {
				if err := configureRegistryMirrors(ctx, cfg.Spec.Registry.Mirrors, r, u); err != nil {
					return err
				}
			}
			if hasProxyConfig(cfg.Spec.Proxy) {
				if err := configureContainerdProxy(ctx, cfg, r, u); err != nil {
					return err
				}
			}
			_, err := r.RunSudo(ctx, "systemctl daemon-reload && systemctl restart containerd")
			return err
		}); err != nil {
			return err
		}
	}

	fmt.Fprintf(e.Out, "\nĐã đồng bộ Addons & cấu hình cụm %q thành công sau %s!\n", cfg.Metadata.Name, time.Since(start).Round(time.Millisecond))
	return nil
}

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

	// 1. Metrics Server (HPA & kubectl top) — nếu bật secureTLS thì dùng kubelet serverTLSBootstrap (không dùng --kubelet-insecure-tls)
	if addons.MetricsServer.Enabled {
		tlsPatch := `if ! grep -q -- '--kubelet-insecure-tls' "$F"; then
  sed -i '/--kubelet-preferred-address-types=/a\        - --kubelet-insecure-tls' "$F"
fi`
		if addons.MetricsServer.SecureTLS {
			fmt.Fprintf(e.Out, "  [addon]     metrics-server (HPA / kubectl top — verified kubelet TLS via serverTLSBootstrap)...\n")
			tlsPatch = `sed -i '/--kubelet-insecure-tls/d' "$F"
for csr in $(/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get csr -o jsonpath='{range .items[?(@.spec.signerName=="kubernetes.io/kubelet-serving")]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true); do
  [ -n "$csr" ] && /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf certificate approve "$csr" >/dev/null 2>&1 || true
done`
		} else {
			fmt.Fprintf(e.Out, "  [addon]     metrics-server (HPA / kubectl top)...\n")
		}
		script := fmt.Sprintf(`set -euo pipefail
F="%s/metrics-server.yaml"
sed -i 's/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g' "$F"
%s
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$F"
`, remoteManifestDir, tlsPatch)
		if _, err := cp1Runner.RunSudo(ctx, script); err != nil {
			return fmt.Errorf("install metrics-server addon: %w", err)
		}
	}

	// 2. Local Path Provisioner (StorageClass mặc định cho PVC động)
	if addons.LocalPathStorage.Enabled {
		fmt.Fprintf(e.Out, "  [addon]     local-path-provisioner (Default StorageClass)...\n")
		busyboxVer := state.BundleManifest.Components["busybox"]
		busyboxPatch := ""
		if busyboxVer != "" {
			busyboxPatch = fmt.Sprintf(`sed -i -E 's|(image:[[:space:]]*)(docker\.io/library/)?busybox([[:space:]]*)$|\1\2busybox:%s\3|g' "$F"`, busyboxVer)
		}
		script := fmt.Sprintf(`set -euo pipefail
F="%s/local-path-storage.yaml"
sed -i 's/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g' "$F"
%s
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$F"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf patch storageclass local-path \
  -p '{"metadata": {"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
`, remoteManifestDir, busyboxPatch)
		if _, err := cp1Runner.RunSudo(ctx, script); err != nil {
			return fmt.Errorf("install local-path-provisioner addon: %w", err)
		}
	}

	// 3. MetalLB (Bare-metal LoadBalancer L2 hoặc BGP) + tự tạo IPAddressPool & Advertisement tương ứng
	if addons.MetalLB.Enabled {
		modeLabel := "L2"
		if addons.MetalLB.BGP != nil && addons.MetalLB.BGP.Enabled {
			modeLabel = fmt.Sprintf("BGP AS%d->AS%d @ %s", addons.MetalLB.BGP.LocalASN, addons.MetalLB.BGP.PeerASN, addons.MetalLB.BGP.PeerIP)
		}
		fmt.Fprintf(e.Out, "  [addon]     metallb (%s LoadBalancer, ipRange=%s)...\n", modeLabel, addons.MetalLB.IPRange)
		poolManifest := renderMetalLBPoolManifest(addons.MetalLB)

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

	// 4. Kong Ingress Controller v3.x (DB-less mode, nhúng sẵn từ Helm chart kong/kong kèm version từ BOM)
	if addons.KongIngress.Enabled {
		kongGW := state.BundleManifest.Components["kong_gateway"]
		kongCtrl := state.BundleManifest.Components["kong_controller"]
		if kongGW == "" || kongCtrl == "" {
			return fmt.Errorf("missing kong_gateway or kong_controller version in BOM")
		}
		fmt.Fprintf(e.Out, "  [addon]     kong-ingress-controller v%s / gateway v%s (DB-less API Gateway)...\n", kongCtrl, kongGW)
		kongYAML := templates.RenderKongDBLess(kongGW, kongCtrl)
		if err := cp1Runner.WriteFile(ctx, kongYAML, ".vkube-tmp/kong-dbless.yaml", 0o600); err != nil {
			return fmt.Errorf("upload kong manifest: %w", err)
		}
		cp1User := state.Config.EffectiveSSH(cp1).User
		script := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
F="$USER_HOME/.vkube-tmp/kong-dbless.yaml"
trap 'rm -f "$F"' EXIT
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply --server-side --force-conflicts -f "$F"
`, cp1User)
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

// renderMetalLBPoolManifest sinh manifest IPAddressPool kèm L2Advertisement (mặc định) hoặc BGPPeer + BGPAdvertisement.
func renderMetalLBPoolManifest(m api.MetalLBAddon) string {
	pool := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: vkube-default-pool
  namespace: metallb-system
spec:
  addresses:
    - %s
`, m.IPRange)
	if m.BGP != nil && m.BGP.Enabled {
		return pool + fmt.Sprintf(`---
apiVersion: metallb.io/v1beta2
kind: BGPPeer
metadata:
  name: vkube-default-bgp-peer
  namespace: metallb-system
spec:
  myASN: %d
  peerASN: %d
  peerAddress: %s
---
apiVersion: metallb.io/v1beta1
kind: BGPAdvertisement
metadata:
  name: vkube-default-bgp
  namespace: metallb-system
spec:
  ipAddressPools:
    - vkube-default-pool
`, m.BGP.LocalASN, m.BGP.PeerASN, m.BGP.PeerIP)
	}
	return pool + `---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: vkube-default-l2
  namespace: metallb-system
spec:
  ipAddressPools:
    - vkube-default-pool
`
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
