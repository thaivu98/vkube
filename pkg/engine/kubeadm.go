package engine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/bundle"
	"github.com/thaivu98/vkube/pkg/ssh"
	"github.com/thaivu98/vkube/pkg/templates"
	"github.com/thaivu98/vkube/pkg/versions"
)

// ApplyOptions chứa các tham số chạy cho lệnh `vkube apply`.
type ApplyOptions struct {
	BundlePath    string // Nếu có -> chế độ Air-gapped; nếu rỗng -> chế độ Online trực tiếp
	ManifestPath  string // File BOM versions.yaml tùy chỉnh (dùng ở chế độ Online)
	KubeconfigOut string
	FastSync      bool // Ép bật LAN HTTP BundleServer cho Air-gapped (mặc định tự bật với >= 3 nodes)
}

// Apply thực thi trọn gói pipeline triển khai cụm Vanilla Kubernetes (hỗ trợ cả Online trực tiếp lẫn Air-gapped Offline Bundle).
func (e *Engine) Apply(ctx context.Context, cfg *api.ClusterConfig, opts ApplyOptions) error {
	start := time.Now()
	onlineMode := strings.TrimSpace(opts.BundlePath) == ""

	var (
		manifest *api.BundleManifest
		bom      *versions.BOM
		err      error
	)

	if onlineMode {
		fmt.Fprintf(e.Out, "==> [0/6] Chế độ Online trực tiếp (không cần Offline Bundle) — đọc BOM...\n")
		bom, err = versions.Load(opts.ManifestPath)
		if err != nil {
			return fmt.Errorf("load BOM manifest: %w", err)
		}
		fmt.Fprintf(e.Out, "  [bom]       %s | Kubernetes %s\n", bom.ReleaseTag, bom.Components["kubernetes"])
	} else {
		fmt.Fprintf(e.Out, "==> [0/6] Chế độ Air-gapped — xác thực Offline Bundle (%s)...\n", opts.BundlePath)
		manifest, err = bundle.Verify(opts.BundlePath)
		if err != nil {
			return fmt.Errorf("verify offline bundle: %w", err)
		}
		fmt.Fprintf(e.Out, "  [bundle]    %s | Kubernetes %s | arch=linux/%s | %d files OK\n",
			manifest.ReleaseTag, manifest.KubernetesVersion, manifest.Arch, len(manifest.Files))
	}

	pool, err := ssh.ConnectAll(ctx, cfg, e.Out)
	if err != nil {
		return err
	}
	defer pool.Close()

	state := &ClusterState{
		Config:         cfg,
		OnlineMode:     onlineMode,
		FastSync:       opts.FastSync,
		BundlePath:     opts.BundlePath,
		BundleManifest: manifest,
		Pool:           pool,
		Facts:          make(map[string]*api.NodeFacts, len(cfg.Spec.Nodes)),
	}

	tStage := time.Now()
	if err := e.RunPreflight(ctx, state); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  [timing]    Preflight: %s\n", time.Since(tStage).Round(time.Millisecond))

	// Ở chế độ Online, sau khi Preflight phát hiện chính xác kiến trúc các node thì render URL tải về từ BOM.
	if onlineMode {
		cp1 := cfg.ControlPlanes()[0]
		detectedArch := state.Facts[cp1.Name].Arch
		resolved, err := bom.Resolve(detectedArch)
		if err != nil {
			return fmt.Errorf("resolve BOM for arch %s: %w", detectedArch, err)
		}
		state.ResolvedBOM = resolved
		state.BundleManifest = &api.BundleManifest{
			ReleaseTag:        resolved.ReleaseTag,
			KubernetesVersion: resolved.KubernetesVersion,
			Arch:              resolved.Arch,
			Components:        resolved.Components,
		}
	}

	tStage = time.Now()
	if err := e.RunOSPrep(ctx, state); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  [timing]    OS Prep: %s\n", time.Since(tStage).Round(time.Millisecond))

	tStage = time.Now()
	if err := e.RunRuntimeAndArtifacts(ctx, state); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  [timing]    Runtime & Artifacts: %s\n", time.Since(tStage).Round(time.Millisecond))

	tStage = time.Now()
	if err := e.runKubeadmCluster(ctx, state); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  [timing]    kubeadm init/join: %s\n", time.Since(tStage).Round(time.Millisecond))

	tStage = time.Now()
	if err := e.runCNIAndPostConfig(ctx, state, opts.KubeconfigOut); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  [timing]    CNI (%s) & Nodes Ready: %s\n", cfg.Spec.Networking.CNI, time.Since(tStage).Round(time.Millisecond))

	tStage = time.Now()
	if err := e.runAddons(ctx, state); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  [timing]    Addons: %s\n", time.Since(tStage).Round(time.Millisecond))

	// Tự động hiển thị trạng thái tổng quan của cụm ngay sau khi triển khai xong.
	cp1 := cfg.ControlPlanes()[0]
	if cp1Runner, err := pool.Get(cp1.Name); err == nil {
		_ = e.printClusterStatus(ctx, cfg.Metadata.Name, cp1, cp1Runner)
	}

	fmt.Fprintf(e.Out, "\nTriển khai cụm %q thành công — Tổng thời gian: %s!\n", cfg.Metadata.Name, time.Since(start).Round(time.Second))
	return nil
}

func (e *Engine) runKubeadmCluster(ctx context.Context, state *ClusterState) error {
	cps := state.Config.ControlPlanes()
	workers := state.Config.Workers()
	cp1 := cps[0]
	cp1Runner, err := state.Pool.Get(cp1.Name)
	if err != nil {
		return err
	}

	isHA := len(cps) > 1
	var certKey string
	if isHA {
		certKey, err = randomHex(32)
		if err != nil {
			return err
		}
	}

	// Nếu bật EncryptSecrets, sinh khóa AES-CBC 32-byte và phân phối đồng bộ tới TOÀN BỘ Control Plane trước khi init/join
	if state.Config.Spec.ControlPlane.EncryptSecrets {
		rawKey := make([]byte, 32)
		if _, err := rand.Read(rawKey); err != nil {
			return fmt.Errorf("generate etcd secret encryption key: %w", err)
		}
		encYAML := templates.RenderEncryptionConfig(base64.StdEncoding.EncodeToString(rawKey))
		if err := state.Pool.RunParallel(ctx, cps, func(ctx context.Context, cp api.NodeConfig, r ssh.Runner) error {
			if err := r.WriteFile(ctx, encYAML, ".vkube-tmp/encryption-config.yaml", 0o600); err != nil {
				return err
			}
			u := state.Config.EffectiveSSH(cp).User
			cmd := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
mkdir -p -m 0700 /etc/kubernetes/enc
install -m 0600 "$USER_HOME/.vkube-tmp/encryption-config.yaml" /etc/kubernetes/enc/encryption-config.yaml
rm -f "$USER_HOME/.vkube-tmp/encryption-config.yaml"`, u)
			_, err := r.RunSudo(ctx, cmd)
			return err
		}); err != nil {
			return fmt.Errorf("distribute etcd EncryptionConfiguration: %w", err)
		}
		fmt.Fprintf(e.Out, "  [security]  Đã kích hoạt mã hóa Secret trong etcd (Encryption at Rest AES-CBC) trên %d Control Plane(s)\n", len(cps))
	}

	// 1. Nếu có cấu hình VIP (kube-vip), ghi sẵn static pod lên CP-1 với super-admin.conf trước khi kubeadm init
	if state.Config.Spec.VIP != "" {
		if err := e.writeKubeVipManifest(ctx, state, cp1, cp1Runner, "/etc/kubernetes/super-admin.conf"); err != nil {
			return err
		}
	}

	// 2. Render & ghi file kubeadm-init.yaml (v1beta4) lên CP-1
	certSANs := make([]string, 0, len(cps)+2)
	if state.Config.Spec.VIP != "" {
		certSANs = append(certSANs, state.Config.Spec.VIP)
	}
	for _, cp := range cps {
		certSANs = append(certSANs, cp.EffectiveNodeIP(), cp.Name)
		if cp.Host != cp.EffectiveNodeIP() {
			certSANs = append(certSANs, cp.Host)
		}
	}

	pullPolicy := "Never"
	if state.OnlineMode {
		pullPolicy = "IfNotPresent"
	}

	initYAML, err := templates.RenderKubeadmInit(templates.KubeadmInitData{
		ClusterName:                state.Config.Metadata.Name,
		KubernetesVersion:          state.BundleManifest.KubernetesVersion,
		AdvertiseAddress:           cp1.EffectiveNodeIP(),
		NodeName:                   cp1.Name,
		ControlPlaneEndpoint:       state.Config.Spec.ControlPlaneEndpoint,
		PodSubnet:                  state.Config.Spec.Networking.PodSubnet,
		ServiceSubnet:              state.Config.Spec.Networking.ServiceSubnet,
		CertificateKey:             certKey,
		CertSANs:                   certSANs,
		ImagePullPolicy:            pullPolicy,
		ImageRepository:            state.Config.Spec.Registry.ImageRepository,
		APIServerExtraArgs:         templates.SortedArgs(state.Config.Spec.ControlPlane.APIServerExtraArgs),
		ControllerManagerExtraArgs: templates.SortedArgs(state.Config.Spec.ControlPlane.ControllerManagerExtraArgs),
		SchedulerExtraArgs:         templates.SortedArgs(state.Config.Spec.ControlPlane.SchedulerExtraArgs),
		EncryptSecrets:             state.Config.Spec.ControlPlane.EncryptSecrets,
		ServerTLSBootstrap:         state.Config.Spec.Addons.MetricsServer.SecureTLS,
		CISHardening:               state.Config.Spec.ControlPlane.CISHardening,
	})
	if err != nil {
		return fmt.Errorf("render kubeadm-init.yaml: %w", err)
	}

	if err := cp1Runner.WriteFile(ctx, initYAML, ".vkube-tmp/kubeadm-init.yaml", 0o600); err != nil {
		return err
	}

	// Nếu bật CISHardening, tạo sẵn thư mục audit log & siết quyền file cấu hình Control Plane chuẩn CIS
	if state.Config.Spec.ControlPlane.CISHardening {
		_ = state.Pool.RunParallel(ctx, cps, func(ctx context.Context, cp api.NodeConfig, r ssh.Runner) error {
			_, err := r.RunSudo(ctx, "mkdir -p -m 0700 /var/log/kubernetes/audit")
			return err
		})
	}

	cp1User := state.Config.EffectiveSSH(cp1).User
	fmt.Fprintf(e.Out, "==> [4/6] Khởi tạo Primary Control Plane (%s @ %s)...\n", cp1.Name, cp1.Host)
	initCmd := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
mkdir -p -m 0700 /etc/vkube
install -m 0600 "$USER_HOME/.vkube-tmp/kubeadm-init.yaml" /etc/vkube/kubeadm-init.yaml
rm -f "$USER_HOME/.vkube-tmp/kubeadm-init.yaml"
trap 'rm -f /etc/vkube/kubeadm-init.yaml' EXIT
/usr/local/bin/kubeadm init --config=/etc/vkube/kubeadm-init.yaml`, cp1User)
	if isHA {
		initCmd += " --upload-certs"
	}
	if state.Config.Spec.Networking.CNI == api.CNICilium && state.Config.Spec.Networking.KubeProxyReplacement {
		initCmd += " --skip-phases=addon/kube-proxy"
	}
	// Trên VM 2GB ARM64, MemTotal khả dụng thường ~1600MB (< ngưỡng mặc định 1700MB của kubeadm).
	if f := state.Facts[cp1.Name]; f != nil && f.MemoryMB > 0 && f.MemoryMB < 1750 {
		initCmd += " --ignore-preflight-errors=Mem"
	}
	if _, err := cp1Runner.RunSudo(ctx, initCmd); err != nil {
		return fmt.Errorf("kubeadm init on %s: %w", cp1.Name, err)
	}

	// Sau khi init xong, đổi kube-vip trên CP-1 từ super-admin.conf về admin.conf chuẩn bảo mật
	if state.Config.Spec.VIP != "" {
		if err := e.writeKubeVipManifest(ctx, state, cp1, cp1Runner, "/etc/kubernetes/admin.conf"); err != nil {
			return err
		}
	}

	// Lấy token/hash từ CP-1 và dùng JoinConfiguration (file 0600) để không bao giờ lộ token/certificateKey trên `ps`
	if len(cps) > 1 || len(workers) > 0 {
		res, err := cp1Runner.RunSudo(ctx, "/usr/local/bin/kubeadm token create --print-join-command")
		if err != nil {
			return fmt.Errorf("create kubeadm join command: %w", err)
		}
		endpoint, token, caHash, err := parseJoinCommand(res.Stdout)
		if err != nil {
			return fmt.Errorf("parse kubeadm join command: %w", err)
		}

		// 3. Join các Secondary Control Planes TUẦN TỰ từng node để đảm bảo an toàn cho etcd Raft quorum
		if len(cps) > 1 {
			fmt.Fprintf(e.Out, "==> [5/6] Join %d Secondary Control Plane(s) (tuần tự để bảo vệ etcd Raft)...\n", len(cps)-1)
			for _, cp := range cps[1:] {
				r, err := state.Pool.Get(cp.Name)
				if err != nil {
					return err
				}
				joinYAML, err := templates.RenderKubeadmJoin(templates.KubeadmJoinData{
					APIServerEndpoint:        endpoint,
					Token:                    token,
					DiscoveryTokenCACertHash: caHash,
					NodeName:                 cp.Name,
					ImagePullPolicy:          pullPolicy,
					IsControlPlane:           true,
					AdvertiseAddress:         cp.EffectiveNodeIP(),
					CertificateKey:           certKey,
				})
				if err != nil {
					return fmt.Errorf("render kubeadm-join.yaml for %s: %w", cp.Name, err)
				}
				fmt.Fprintf(e.Out, "  [join-cp]   %-12s joining cluster...\n", cp.Name)
				if err := e.execKubeadmJoin(ctx, state, cp, r, joinYAML); err != nil {
					return fmt.Errorf("kubeadm join control-plane on %s: %w", cp.Name, err)
				}
				if state.Config.Spec.VIP != "" {
					if err := e.writeKubeVipManifest(ctx, state, cp, r, "/etc/kubernetes/admin.conf"); err != nil {
						return err
					}
				}
				fmt.Fprintf(e.Out, "  [join-cp]   %-12s OK\n", cp.Name)
			}
		}

		// 4. Join toàn bộ Worker Nodes SONG SONG 100%
		if len(workers) > 0 {
			fmt.Fprintf(e.Out, "==> [5/6] Join %d Worker Node(s) song song...\n", len(workers))
			err := state.Pool.RunParallel(ctx, workers, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
				joinYAML, err := templates.RenderKubeadmJoin(templates.KubeadmJoinData{
					APIServerEndpoint:        endpoint,
					Token:                    token,
					DiscoveryTokenCACertHash: caHash,
					NodeName:                 node.Name,
					ImagePullPolicy:          pullPolicy,
					IsControlPlane:           false,
				})
				if err != nil {
					return fmt.Errorf("render kubeadm-join.yaml: %w", err)
				}
				if err := e.execKubeadmJoin(ctx, state, node, r, joinYAML); err != nil {
					return fmt.Errorf("kubeadm join worker: %w", err)
				}
				fmt.Fprintf(e.Out, "  [join-wk]   %-12s OK\n", node.Name)
				return nil
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (e *Engine) execKubeadmJoin(ctx context.Context, state *ClusterState, node api.NodeConfig, r ssh.Runner, joinYAML []byte) error {
	if err := r.WriteFile(ctx, joinYAML, ".vkube-tmp/kubeadm-join.yaml", 0o600); err != nil {
		return err
	}
	sshUser := state.Config.EffectiveSSH(node).User
	joinFlags := ""
	if node.Role == api.RoleControlPlane {
		if f := state.Facts[node.Name]; f != nil && f.MemoryMB > 0 && f.MemoryMB < 1750 {
			joinFlags = " --ignore-preflight-errors=Mem"
		}
	}
	cmd := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
mkdir -p -m 0700 /etc/vkube
install -m 0600 "$USER_HOME/.vkube-tmp/kubeadm-join.yaml" /etc/vkube/kubeadm-join.yaml
rm -f "$USER_HOME/.vkube-tmp/kubeadm-join.yaml"
trap 'rm -f /etc/vkube/kubeadm-join.yaml' EXIT
/usr/local/bin/kubeadm join --config=/etc/vkube/kubeadm-join.yaml%s`, sshUser, joinFlags)
	_, err := r.RunSudo(ctx, cmd)
	return err
}

// parseJoinCommand tách apiServerEndpoint, token và discovery-token-ca-cert-hash từ output của `kubeadm token create --print-join-command`.
func parseJoinCommand(out string) (endpoint, token, caHash string, err error) {
	fields := strings.Fields(strings.TrimSpace(out))
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "join":
			if i+1 < len(fields) && endpoint == "" {
				endpoint = fields[i+1]
			}
		case "--token":
			if i+1 < len(fields) {
				token = fields[i+1]
			}
		case "--discovery-token-ca-cert-hash":
			if i+1 < len(fields) {
				caHash = fields[i+1]
			}
		}
	}
	if endpoint == "" || token == "" || caHash == "" {
		return "", "", "", fmt.Errorf("unexpected kubeadm join output: %q", strings.TrimSpace(out))
	}
	return endpoint, token, caHash, nil
}

func (e *Engine) writeKubeVipManifest(ctx context.Context, state *ClusterState, node api.NodeConfig, r ssh.Runner, kubeconfigPath string) error {
	iface := state.Config.Spec.VIPInterface
	if iface == "" && state.Facts[node.Name] != nil {
		iface = state.Facts[node.Name].DefaultInterface
	}
	if iface == "" {
		return fmt.Errorf("cannot determine network interface for kube-vip on node %s (set spec.vipInterface)", node.Name)
	}
	kubeVipVer := state.BundleManifest.Components["kube_vip"]
	if kubeVipVer == "" {
		return fmt.Errorf("bundle manifest missing kube_vip component version")
	}

	pullPolicy := "Never"
	if state.OnlineMode {
		pullPolicy = "IfNotPresent"
	}
	vipData := templates.KubeVipData{
		Image:           fmt.Sprintf("ghcr.io/kube-vip/kube-vip:%s", kubeVipVer),
		VIP:             state.Config.Spec.VIP,
		Interface:       iface,
		KubeconfigPath:  kubeconfigPath,
		ImagePullPolicy: pullPolicy,
	}
	if bgp := state.Config.Spec.VIPBGP; bgp != nil && bgp.Enabled {
		vipData.BGPEnable = true
		vipData.BGPAS = bgp.LocalASN
		vipData.BGPPeerAS = bgp.PeerASN
		vipData.BGPPeerAddress = bgp.PeerIP
	}
	manifestBytes, err := templates.RenderKubeVip(vipData)
	if err != nil {
		return err
	}
	if err := r.WriteFile(ctx, manifestBytes, ".vkube-tmp/kube-vip.yaml", 0o600); err != nil {
		return err
	}
	sshUser := state.Config.EffectiveSSH(node).User
	_, err = r.RunSudo(ctx, fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
mkdir -p /etc/kubernetes/manifests
install -m 0600 "$USER_HOME/.vkube-tmp/kube-vip.yaml" /etc/kubernetes/manifests/kube-vip.yaml
rm -f "$USER_HOME/.vkube-tmp/kube-vip.yaml"`, sshUser))
	return err
}

func (e *Engine) runCNIAndPostConfig(ctx context.Context, state *ClusterState, kubeconfigOut string) error {
	mode := "Air-gapped"
	if state.OnlineMode {
		mode = "Online"
	}
	cni := state.Config.Spec.Networking.CNI
	fmt.Fprintf(e.Out, "==> [6/6] Cài đặt %s CNI (%s), áp dụng Labels/Taints & xuất kubeconfig...\n", strings.ToUpper(cni), mode)
	cp1 := state.Config.ControlPlanes()[0]
	cp1Runner, err := state.Pool.Get(cp1.Name)
	if err != nil {
		return err
	}

	if cni == api.CNICilium {
		ciliumVer := state.BundleManifest.Components["cilium"]
		if ciliumVer == "" {
			return fmt.Errorf("BOM missing cilium component version")
		}
		k8sHost := cp1.Host
		k8sPort := "6443"
		if state.Config.Spec.ControlPlaneEndpoint != "" {
			if h, p, err := net.SplitHostPort(state.Config.Spec.ControlPlaneEndpoint); err == nil {
				k8sHost, k8sPort = h, p
			}
		} else if state.Config.Spec.VIP != "" {
			k8sHost = state.Config.Spec.VIP
		}
		ciliumYAML := templates.RenderCilium(templates.CiliumData{
			Version:              ciliumVer,
			PodSubnet:            state.Config.Spec.Networking.PodSubnet,
			KubeProxyReplacement: state.Config.Spec.Networking.KubeProxyReplacement,
			K8sServiceHost:       k8sHost,
			K8sServicePort:       k8sPort,
		})
		if err := cp1Runner.WriteFile(ctx, ciliumYAML, ".vkube-tmp/cilium.yaml", 0o600); err != nil {
			return fmt.Errorf("upload cilium manifest: %w", err)
		}
		cp1User := state.Config.EffectiveSSH(cp1).User
		ciliumCmd := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
F="$USER_HOME/.vkube-tmp/cilium.yaml"
trap 'rm -f "$F"' EXIT
for attempt in 1 2 3; do
  if /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply --server-side --force-conflicts -f "$F"; then
    exit 0
  fi
  sleep 5
done
exit 1`, cp1User)
		if _, err := cp1Runner.RunSudo(ctx, ciliumCmd); err != nil {
			return fmt.Errorf("apply Cilium eBPF CNI: %w", err)
		}
	} else {
		// Patch Calico manifest và xác minh bằng grep rằng PodSubnet đã được ghi đúng trước khi apply (tránh fallback âm thầm về 192.168.0.0/16)
		calicoScript := fmt.Sprintf(`set -euo pipefail
CALICO_FILE="%s/calico.yaml"
sed -i 's/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g' "$CALICO_FILE"
sed -i 's|# - name: CALICO_IPV4POOL_CIDR|- name: CALICO_IPV4POOL_CIDR|g' "$CALICO_FILE"
sed -i 's|#   value: "192.168.0.0/16"|  value: "%s"|g' "$CALICO_FILE"
if ! grep -qF 'value: "%s"' "$CALICO_FILE"; then
  echo "ERROR: failed to patch CALICO_IPV4POOL_CIDR=%s into $CALICO_FILE" >&2
  exit 1
fi
for attempt in 1 2 3; do
  if /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply --server-side --force-conflicts -f "$CALICO_FILE"; then
    exit 0
  fi
  sleep 5
done
exit 1
`, remoteManifestDir, state.Config.Spec.Networking.PodSubnet, state.Config.Spec.Networking.PodSubnet, state.Config.Spec.Networking.PodSubnet)

		if _, err := cp1Runner.RunSudo(ctx, calicoScript); err != nil {
			return fmt.Errorf("apply Calico CNI: %w", err)
		}
	}

	// Áp dụng Labels, Taints và chính sách bảo vệ Control Plane
	for _, node := range state.Config.Spec.Nodes {
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

	if state.Config.Spec.AllowSchedulingOnControlPlane {
		for _, cp := range state.Config.ControlPlanes() {
			cmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf taint node %s node-role.kubernetes.io/control-plane:NoSchedule- || true", cp.Name)
			_, _ = cp1Runner.RunSudo(ctx, cmd)
		}
	}

	// Cài sẵn ~/.kube/config cho user SSH trên Primary Control Plane để gõ `kubectl` chạy ngay
	cp1User := state.Config.EffectiveSSH(cp1).User
	setupKubeCmd := fmt.Sprintf(`set -euo pipefail
HOME_DIR="$(getent passwd %s | cut -d: -f6)"
if [ -n "$HOME_DIR" ]; then
  mkdir -p "$HOME_DIR/.kube"
  cp -f /etc/kubernetes/admin.conf "$HOME_DIR/.kube/config"
  chown -R %s:%s "$HOME_DIR/.kube"
  chmod 0600 "$HOME_DIR/.kube/config"
fi`, cp1User, cp1User, cp1User)
	_, _ = cp1Runner.RunSudo(ctx, setupKubeCmd)

	// Kéo file kubeconfig về máy Deployer và chuẩn hóa context name theo đúng metadata.name của cụm
	res, err := cp1Runner.RunSudo(ctx, "cat /etc/kubernetes/admin.conf")
	if err != nil {
		return fmt.Errorf("fetch admin.conf from %s: %w", cp1.Name, err)
	}
	kubeconfigContent := strings.ReplaceAll(res.Stdout,
		fmt.Sprintf("kubernetes-admin@%s", state.Config.Metadata.Name),
		state.Config.Metadata.Name,
	)
	if kubeconfigOut == "" {
		kubeconfigOut = fmt.Sprintf("kubeconfig-%s.yaml", state.Config.Metadata.Name)
	}
	if dir := filepath.Dir(kubeconfigOut); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(kubeconfigOut, []byte(kubeconfigContent), 0o600); err != nil {
		return fmt.Errorf("write local kubeconfig %q: %w", kubeconfigOut, err)
	}
	fmt.Fprintf(e.Out, "  [kubeconfig] Đã lưu kubeconfig tại: %s (context: %s)\n", kubeconfigOut, state.Config.Metadata.Name)

	// Nếu bật serverTLSBootstrap (metricsServer.secureTLS), tự động duyệt các CSR kubelet-serving đang chờ
	if state.Config.Spec.Addons.MetricsServer.SecureTLS {
		approveCSR := `set -euo pipefail
for csr in $(/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get csr -o jsonpath='{range .items[?(@.spec.signerName=="kubernetes.io/kubelet-serving")]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true); do
  [ -n "$csr" ] && /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf certificate approve "$csr" >/dev/null 2>&1 || true
done`
		_, _ = cp1Runner.RunSudo(ctx, approveCSR)
	}

	// Siết quyền file manifest & PKI theo CIS Benchmark khi bật cisHardening
	if state.Config.Spec.ControlPlane.CISHardening {
		_ = state.Pool.RunParallel(ctx, state.Config.ControlPlanes(), func(ctx context.Context, cp api.NodeConfig, r ssh.Runner) error {
			_, err := r.RunSudo(ctx, `chmod 0600 /etc/kubernetes/manifests/*.yaml /etc/kubernetes/*.conf 2>/dev/null || true
chmod 0700 /var/lib/etcd 2>/dev/null || true`)
			return err
		})
	}

	// Chờ toàn bộ nodes đạt trạng thái Ready (tối đa 3 phút)
	fmt.Fprintf(e.Out, "  [verify]    Đang chờ toàn bộ %d node(s) đạt trạng thái Ready...\n", len(state.Config.Spec.Nodes))
	waitCmd := "/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf wait --for=condition=Ready nodes --all --timeout=180s"
	if _, err := cp1Runner.RunSudo(ctx, waitCmd); err != nil {
		return fmt.Errorf("wait for nodes Ready: %w", err)
	}
	return nil
}

func randomHex(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
