package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/bundle"
	"github.com/thaivu98/vkube/pkg/ssh"
	"github.com/thaivu98/vkube/pkg/templates"
)

// Apply thực thi trọn gói pipeline triển khai cụm Vanilla Kubernetes từ Offline Bundle.
func (e *Engine) Apply(ctx context.Context, cfg *api.ClusterConfig, bundlePath, kubeconfigOut string) error {
	start := time.Now()

	fmt.Fprintf(e.Out, "==> [0/6] Xác thực Offline Bundle (%s)...\n", bundlePath)
	manifest, err := bundle.Verify(bundlePath)
	if err != nil {
		return fmt.Errorf("verify offline bundle: %w", err)
	}
	fmt.Fprintf(e.Out, "  [bundle]    %s | Kubernetes %s | arch=linux/%s | %d files OK\n",
		manifest.ReleaseTag, manifest.KubernetesVersion, manifest.Arch, len(manifest.Files))

	pool, err := ssh.ConnectAll(ctx, cfg, e.Out)
	if err != nil {
		return err
	}
	defer pool.Close()

	state := &ClusterState{
		Config:         cfg,
		BundlePath:     bundlePath,
		BundleManifest: manifest,
		Pool:           pool,
		Facts:          make(map[string]*api.NodeFacts, len(cfg.Spec.Nodes)),
	}

	if err := e.RunPreflight(ctx, state); err != nil {
		return err
	}
	if err := e.RunOSPrep(ctx, state); err != nil {
		return err
	}
	if err := e.RunRuntimeAndArtifacts(ctx, state); err != nil {
		return err
	}
	if err := e.runKubeadmCluster(ctx, state); err != nil {
		return err
	}
	if err := e.runCalicoAndPostConfig(ctx, state, kubeconfigOut); err != nil {
		return err
	}

	fmt.Fprintf(e.Out, "\nTriển khai cụm %q thành công sau %s!\n", cfg.Metadata.Name, time.Since(start).Round(time.Second))
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
		certSANs = append(certSANs, cp.Host, cp.Name)
	}

	initYAML, err := templates.RenderKubeadmInit(templates.KubeadmInitData{
		ClusterName:          state.Config.Metadata.Name,
		KubernetesVersion:    state.BundleManifest.KubernetesVersion,
		AdvertiseAddress:     cp1.Host,
		NodeName:             cp1.Name,
		ControlPlaneEndpoint: state.Config.Spec.ControlPlaneEndpoint,
		PodSubnet:            state.Config.Spec.Networking.PodSubnet,
		ServiceSubnet:        state.Config.Spec.Networking.ServiceSubnet,
		CertificateKey:       certKey,
		CertSANs:             certSANs,
	})
	if err != nil {
		return fmt.Errorf("render kubeadm-init.yaml: %w", err)
	}

	if err := cp1Runner.WriteFile(ctx, initYAML, "/tmp/vkube-kubeadm-init.yaml", 0o600); err != nil {
		return err
	}

	fmt.Fprintf(e.Out, "==> [4/6] Khởi tạo Primary Control Plane (%s @ %s)...\n", cp1.Name, cp1.Host)
	initCmd := "mkdir -p /etc/vkube && mv -f /tmp/vkube-kubeadm-init.yaml /etc/vkube/kubeadm-init.yaml && /usr/local/bin/kubeadm init --config=/etc/vkube/kubeadm-init.yaml"
	if isHA {
		initCmd += " --upload-certs"
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

	// Lấy lệnh kubeadm join nếu cụm có thêm CP phụ hoặc Worker
	if len(cps) > 1 || len(workers) > 0 {
		res, err := cp1Runner.RunSudo(ctx, "/usr/local/bin/kubeadm token create --print-join-command")
		if err != nil {
			return fmt.Errorf("create kubeadm join command: %w", err)
		}
		baseJoinCmd := strings.TrimSpace(res.Stdout)

		// 3. Join các Secondary Control Planes TUẦN TỰ từng node để đảm bảo an toàn cho etcd Raft quorum
		if len(cps) > 1 {
			fmt.Fprintf(e.Out, "==> [5/6] Join %d Secondary Control Plane(s) (tuần tự để bảo vệ etcd Raft)...\n", len(cps)-1)
			for _, cp := range cps[1:] {
				r, err := state.Pool.Get(cp.Name)
				if err != nil {
					return err
				}
				joinCmd := fmt.Sprintf("%s --control-plane --certificate-key %s --node-name %s --apiserver-advertise-address %s --cri-socket unix:///run/containerd/containerd.sock",
					baseJoinCmd, certKey, cp.Name, cp.Host)
				fmt.Fprintf(e.Out, "  [join-cp]   %-12s joining cluster...\n", cp.Name)
				if _, err := r.RunSudo(ctx, joinCmd); err != nil {
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
				joinCmd := fmt.Sprintf("%s --node-name %s --cri-socket unix:///run/containerd/containerd.sock",
					baseJoinCmd, node.Name)
				if _, err := r.RunSudo(ctx, joinCmd); err != nil {
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

	manifestBytes, err := templates.RenderKubeVip(templates.KubeVipData{
		Image:          fmt.Sprintf("ghcr.io/kube-vip/kube-vip:%s", kubeVipVer),
		VIP:            state.Config.Spec.VIP,
		Interface:      iface,
		KubeconfigPath: kubeconfigPath,
	})
	if err != nil {
		return err
	}
	if err := r.WriteFile(ctx, manifestBytes, "/tmp/vkube-kube-vip.yaml", 0o600); err != nil {
		return err
	}
	_, err = r.RunSudo(ctx, "mkdir -p /etc/kubernetes/manifests && mv -f /tmp/vkube-kube-vip.yaml /etc/kubernetes/manifests/kube-vip.yaml")
	return err
}

func (e *Engine) runCalicoAndPostConfig(ctx context.Context, state *ClusterState, kubeconfigOut string) error {
	fmt.Fprintf(e.Out, "==> [6/6] Cài đặt Calico CNI offline, áp dụng Labels/Taints & xuất kubeconfig...\n")
	cp1 := state.Config.ControlPlanes()[0]
	cp1Runner, err := state.Pool.Get(cp1.Name)
	if err != nil {
		return err
	}

	// Patch Calico manifest tại chỗ trên CP-1 để khớp PodSubnet và imagePullPolicy: IfNotPresent (100% offline)
	calicoScript := fmt.Sprintf(`set -euo pipefail
CALICO_FILE="%s/calico.yaml"
sed -i 's/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g' "$CALICO_FILE"
sed -i 's|# - name: CALICO_IPV4POOL_CIDR|- name: CALICO_IPV4POOL_CIDR|g' "$CALICO_FILE"
sed -i 's|#   value: "192.168.0.0/16"|  value: "%s"|g' "$CALICO_FILE"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f "$CALICO_FILE"
`, remoteManifestDir, state.Config.Spec.Networking.PodSubnet)

	if _, err := cp1Runner.RunSudo(ctx, calicoScript); err != nil {
		return fmt.Errorf("apply offline Calico CNI: %w", err)
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

	// Kéo file kubeconfig về máy Deployer
	res, err := cp1Runner.RunSudo(ctx, "cat /etc/kubernetes/admin.conf")
	if err != nil {
		return fmt.Errorf("fetch admin.conf from %s: %w", cp1.Name, err)
	}
	if kubeconfigOut == "" {
		kubeconfigOut = fmt.Sprintf("kubeconfig-%s.yaml", state.Config.Metadata.Name)
	}
	if dir := filepath.Dir(kubeconfigOut); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(kubeconfigOut, []byte(res.Stdout), 0o600); err != nil {
		return fmt.Errorf("write local kubeconfig %q: %w", kubeconfigOut, err)
	}
	fmt.Fprintf(e.Out, "  [kubeconfig] Đã lưu kubeconfig tại: %s\n", kubeconfigOut)

	// Chờ toàn bộ nodes đạt trạng thái Ready (tối đa 3 phút)
	fmt.Fprintf(e.Out, "  [verify]    Đang chờ toàn bộ %d node(s) đạt trạng thái Ready...\n", len(state.Config.Spec.Nodes))
	waitCmd := "/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf wait --for=condition=Ready nodes --all --timeout=180s"
	if _, err := cp1Runner.RunSudo(ctx, waitCmd); err != nil {
		return fmt.Errorf("wait for nodes Ready: %w", err)
	}

	nodesOut, _ := cp1Runner.RunSudo(ctx, "/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes -o wide")
	if nodesOut.Stdout != "" {
		fmt.Fprintf(e.Out, "\n%s", nodesOut.Stdout)
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
