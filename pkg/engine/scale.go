package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/bundle"
	"github.com/thaivu98/vkube/pkg/ssh"
	"github.com/thaivu98/vkube/pkg/templates"
	"github.com/thaivu98/vkube/pkg/versions"
)

var validNodeNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// AddNodeOptions chứa tham số cho lệnh `vkube add-node`.
type AddNodeOptions struct {
	NodeName     string // Nếu rỗng -> tự động phát hiện toàn bộ node trong cluster.yaml chưa có trong cụm
	BundlePath   string // Nếu có -> Air-gapped; nếu rỗng -> Online
	ManifestPath string // File BOM tùy chỉnh (chế độ Online)
	FastSync     bool   // Ép bật LAN HTTP BundleServer
}

// RemoveNodeOptions chứa tham số cho lệnh `vkube remove-node`.
type RemoveNodeOptions struct {
	NodeName  string
	SkipReset bool // Chỉ drain & delete khỏi API/etcd, không SSH reset máy đích (hữu ích khi máy đã hỏng phần cứng)
}

// AddNodes mở rộng cụm đang chạy bằng cách chuẩn bị OS/Runtime và join thêm Worker hoặc Control Plane mới.
func (e *Engine) AddNodes(ctx context.Context, cfg *api.ClusterConfig, opts AddNodeOptions) error {
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

	cp1 := cfg.ControlPlanes()[0]
	cp1Client, err := ssh.Dial(ctx, cp1.Name, cp1.Host, cfg.EffectiveSSH(cp1))
	if err != nil {
		return fmt.Errorf("connect primary control-plane %s: %w", cp1.Name, err)
	}
	defer cp1Client.Close()

	// Đọc danh sách node đang tồn tại trong cụm
	res, err := cp1Client.RunSudo(ctx, `/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes -o jsonpath='{.items[*].metadata.name}'`)
	if err != nil {
		return fmt.Errorf("query existing cluster nodes on %s: %w", cp1.Name, err)
	}
	existing := make(map[string]struct{})
	for _, n := range strings.Fields(res.Stdout) {
		existing[n] = struct{}{}
	}

	newNodes, err := selectNodesToAdd(cfg.Spec.Nodes, existing, opts.NodeName)
	if err != nil {
		return err
	}
	if len(newNodes) == 0 {
		fmt.Fprintf(e.Out, "Toàn bộ %d node trong cluster.yaml đã tồn tại trong cụm %q — không có node mới để thêm.\n", len(cfg.Spec.Nodes), cfg.Metadata.Name)
		return nil
	}

	fmt.Fprintf(e.Out, "==> Mở rộng cụm %q: chuẩn bị thêm %d node mới...\n", cfg.Metadata.Name, len(newNodes))

	// Kết nối SSH tới các node mới
	subCfg := *cfg
	subCfg.Spec.Nodes = newNodes
	pool, err := ssh.ConnectAll(ctx, &subCfg, e.Out)
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
		Facts:          make(map[string]*api.NodeFacts, len(newNodes)),
	}

	// 1. Preflight trên các node mới (không check VIP vì VIP đang chạy trên cụm)
	if err := e.runPreflightNodes(ctx, state, newNodes, false); err != nil {
		return err
	}
	if onlineMode {
		detectedArch := state.Facts[newNodes[0].Name].Arch
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

	// 2. OS Prep trên các node mới + đồng bộ /etc/hosts trên CP-1 để phân giải được node mới
	if err := e.runOSPrepNodes(ctx, state, newNodes); err != nil {
		return err
	}
	hostsBlock := buildHostsBlock(cfg.Spec.Nodes)
	_, _ = cp1Client.RunSudo(ctx, fmt.Sprintf(`set -euo pipefail
sed -i '/%s/,/%s/d' /etc/hosts
cat <<'EOF' >> /etc/hosts
%s
EOF`, hostsMarkerStart, hostsMarkerEnd, hostsBlock))

	// 3. Cài đặt Runtime & Artifacts trên các node mới
	if err := e.runRuntimeNodes(ctx, state, newNodes); err != nil {
		return err
	}

	// 4. Tạo join token mới trên CP-1 (và upload-certs nếu có Control Plane mới)
	joinRes, err := cp1Client.RunSudo(ctx, "/usr/local/bin/kubeadm token create --print-join-command")
	if err != nil {
		return fmt.Errorf("create kubeadm join token: %w", err)
	}
	endpoint, token, caHash, err := parseJoinCommand(joinRes.Stdout)
	if err != nil {
		return fmt.Errorf("parse kubeadm join command: %w", err)
	}

	var newCPs, newWorkers []api.NodeConfig
	for _, n := range newNodes {
		if n.Role == api.RoleControlPlane {
			newCPs = append(newCPs, n)
		} else {
			newWorkers = append(newWorkers, n)
		}
	}

	pullPolicy := "Never"
	if state.OnlineMode {
		pullPolicy = "IfNotPresent"
	}

	if len(newCPs) > 0 {
		certRes, err := cp1Client.RunSudo(ctx, "/usr/local/bin/kubeadm init phase upload-certs --upload-certs")
		if err != nil {
			return fmt.Errorf("upload-certs on %s: %w", cp1.Name, err)
		}
		certKey, err := parseUploadCertsKey(certRes.Stdout)
		if err != nil {
			return err
		}

		fmt.Fprintf(e.Out, "==> Join %d Control Plane mới (tuần tự để bảo vệ etcd Raft)...\n", len(newCPs))
		for _, cp := range newCPs {
			r, err := pool.Get(cp.Name)
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
				AdvertiseAddress:         cp.Host,
				CertificateKey:           certKey,
			})
			if err != nil {
				return fmt.Errorf("render kubeadm-join.yaml for %s: %w", cp.Name, err)
			}
			fmt.Fprintf(e.Out, "  [add-cp]    %-12s joining cluster...\n", cp.Name)
			if err := e.execKubeadmJoin(ctx, state, cp, r, joinYAML); err != nil {
				return fmt.Errorf("kubeadm join control-plane on %s: %w", cp.Name, err)
			}
			if cfg.Spec.VIP != "" {
				if err := e.writeKubeVipManifest(ctx, state, cp, r, "/etc/kubernetes/admin.conf"); err != nil {
					return err
				}
			}
			fmt.Fprintf(e.Out, "  [add-cp]    %-12s OK\n", cp.Name)
		}
	}

	if len(newWorkers) > 0 {
		fmt.Fprintf(e.Out, "==> Join %d Worker Node mới song song...\n", len(newWorkers))
		if err := pool.RunParallel(ctx, newWorkers, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
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
			fmt.Fprintf(e.Out, "  [add-wk]    %-12s OK\n", node.Name)
			return nil
		}); err != nil {
			return err
		}
	}

	// 5. Áp dụng Labels & Taints cho các node mới và chờ Ready
	for _, node := range newNodes {
		for k, v := range node.Labels {
			cmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf label node %s %s=%s --overwrite", node.Name, k, v)
			if _, err := cp1Client.RunSudo(ctx, cmd); err != nil {
				return fmt.Errorf("apply label %s=%s on %s: %w", k, v, node.Name, err)
			}
		}
		for _, t := range node.Taints {
			taintSpec := fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect)
			if t.Value == "" {
				taintSpec = fmt.Sprintf("%s:%s", t.Key, t.Effect)
			}
			cmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf taint node %s %s --overwrite", node.Name, taintSpec)
			if _, err := cp1Client.RunSudo(ctx, cmd); err != nil {
				return fmt.Errorf("apply taint %s on %s: %w", taintSpec, node.Name, err)
			}
		}
		if node.Role == api.RoleControlPlane && cfg.Spec.AllowSchedulingOnControlPlane {
			_, _ = cp1Client.RunSudo(ctx, fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf taint node %s node-role.kubernetes.io/control-plane:NoSchedule- || true", node.Name))
		}
	}

	nodeNames := make([]string, 0, len(newNodes))
	for _, n := range newNodes {
		nodeNames = append(nodeNames, n.Name)
	}
	fmt.Fprintf(e.Out, "  [verify]    Đang chờ node mới (%s) đạt trạng thái Ready...\n", strings.Join(nodeNames, ", "))
	waitCmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf wait --for=condition=Ready node/%s --timeout=180s", strings.Join(nodeNames, " node/"))
	if _, err := cp1Client.RunSudo(ctx, waitCmd); err != nil {
		return fmt.Errorf("wait for added nodes Ready: %w", err)
	}

	fmt.Fprintf(e.Out, "\nĐã thêm thành công %d node mới vào cụm %q sau %s!\n", len(newNodes), cfg.Metadata.Name, time.Since(start).Round(time.Second))
	return nil
}

// RemoveNode thực hiện drain workload, gỡ etcd member (nếu là Control Plane), xóa node khỏi Kubernetes API
// và dọn sạch cấu hình Kubernetes trên node bị gỡ (trừ khi --skip-reset).
func (e *Engine) RemoveNode(ctx context.Context, cfg *api.ClusterConfig, opts RemoveNodeOptions) error {
	start := time.Now()
	targetName := strings.TrimSpace(opts.NodeName)
	if targetName == "" {
		return fmt.Errorf("node name is required (use --node <name>)")
	}
	if len(targetName) > 253 || !validNodeNameRe.MatchString(targetName) {
		return fmt.Errorf("invalid node name %q (must be a valid lowercase DNS-1123 name)", targetName)
	}

	// Tìm Control Plane còn sống (khác node đang bị gỡ) để điều phối drain/etcd/delete
	activeCP, err := pickActiveControlPlane(cfg, targetName)
	if err != nil {
		return err
	}

	var targetNode *api.NodeConfig
	for i := range cfg.Spec.Nodes {
		if cfg.Spec.Nodes[i].Name == targetName {
			targetNode = &cfg.Spec.Nodes[i]
			break
		}
	}
	if targetNode == nil && !opts.SkipReset {
		return fmt.Errorf("node %q not found in cluster.yaml (pass --skip-reset to only drain & delete from Kubernetes API)", targetName)
	}

	cpClient, err := ssh.Dial(ctx, activeCP.Name, activeCP.Host, cfg.EffectiveSSH(activeCP))
	if err != nil {
		return fmt.Errorf("connect active control-plane %s: %w", activeCP.Name, err)
	}
	defer cpClient.Close()

	fmt.Fprintf(e.Out, "==> Đang gỡ node %q khỏi cụm %q (điều phối qua %s)...\n", targetName, cfg.Metadata.Name, activeCP.Name)

	// 1. Drain node an toàn
	fmt.Fprintf(e.Out, "  [drain]     Đang di tản toàn bộ Pod trên %s...\n", targetName)
	drainCmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf drain %s --ignore-daemonsets --delete-emptydir-data --force --timeout=120s", targetName)
	if _, err := cpClient.RunSudo(ctx, drainCmd); err != nil {
		return fmt.Errorf("drain node %s: %w", targetName, err)
	}

	// 2. Nếu là Control Plane, kiểm tra tác động quorum & gỡ etcd member ra khỏi Raft quorum trước khi xóa node
	isCP := targetNode != nil && targetNode.Role == api.RoleControlPlane
	if isCP {
		remainingCPs := len(cfg.ControlPlanes()) - 1
		if remainingCPs > 0 && remainingCPs%2 == 0 {
			fmt.Fprintf(e.Out, "  [WARN]      Sau khi gỡ %s, cụm chỉ còn %d Control Plane (số chẵn) — nên bổ sung CP thay thế để đảm bảo etcd Raft quorum.\n", targetName, remainingCPs)
		}
		fmt.Fprintf(e.Out, "  [etcd]      Kiểm tra & gỡ etcd member của %s khỏi Raft quorum...\n", targetName)
		listCmd := `set -euo pipefail
ETCD_CID="$(/usr/local/bin/crictl ps --name '^etcd$' -q | head -n 1)"
/usr/local/bin/crictl exec "$ETCD_CID" etcdctl --endpoints=https://127.0.0.1:2379 --cacert=/etc/kubernetes/pki/etcd/ca.crt --cert=/etc/kubernetes/pki/etcd/server.crt --key=/etc/kubernetes/pki/etcd/server.key member list`
		listRes, err := cpClient.RunSudo(ctx, listCmd)
		if err != nil {
			return fmt.Errorf("list etcd members on %s: %w", activeCP.Name, err)
		}
		if memberID := parseEtcdMemberID(listRes.Stdout, targetName, targetNode.EffectiveNodeIP()); memberID != "" {
			rmCmd := fmt.Sprintf(`set -euo pipefail
ETCD_CID="$(/usr/local/bin/crictl ps --name '^etcd$' -q | head -n 1)"
/usr/local/bin/crictl exec "$ETCD_CID" etcdctl --endpoints=https://127.0.0.1:2379 --cacert=/etc/kubernetes/pki/etcd/ca.crt --cert=/etc/kubernetes/pki/etcd/server.crt --key=/etc/kubernetes/pki/etcd/server.key member remove %s`, memberID)
			if _, err := cpClient.RunSudo(ctx, rmCmd); err != nil {
				return fmt.Errorf("remove etcd member %s (%s): %w", memberID, targetName, err)
			}
			fmt.Fprintf(e.Out, "  [etcd]      Đã gỡ etcd member %s (%s) OK\n", memberID, targetName)
		}
	}

	// 3. Xóa đối tượng Node khỏi Kubernetes API
	fmt.Fprintf(e.Out, "  [delete]    Xóa node %s khỏi Kubernetes API...\n", targetName)
	delCmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf delete node %s", targetName)
	if _, err := cpClient.RunSudo(ctx, delCmd); err != nil {
		return fmt.Errorf("delete node %s: %w", targetName, err)
	}

	// 4. SSH vào node bị gỡ để chạy kubeadm reset & dọn sạch tài nguyên (trừ khi --skip-reset)
	if !opts.SkipReset && targetNode != nil {
		fmt.Fprintf(e.Out, "  [reset]     Dọn sạch Kubernetes/CNI trên %s (%s)...\n", targetNode.Name, targetNode.Host)
		nodeClient, err := ssh.Dial(ctx, targetNode.Name, targetNode.Host, cfg.EffectiveSSH(*targetNode))
		if err != nil {
			return fmt.Errorf("node %s đã bị xóa khỏi cụm nhưng không thể SSH để reset (%w); có thể chạy thủ công hoặc bỏ qua", targetName, err)
		}
		defer nodeClient.Close()
		if _, err := nodeClient.RunSudo(ctx, buildNodeResetScript(cfg.Spec.VIP)); err != nil {
			return fmt.Errorf("reset removed node %s: %w", targetName, err)
		}
	}

	fmt.Fprintf(e.Out, "\nĐã gỡ hoàn toàn node %q khỏi cụm %q sau %s!\n", targetName, cfg.Metadata.Name, time.Since(start).Round(time.Millisecond))
	return nil
}

func selectNodesToAdd(allNodes []api.NodeConfig, existing map[string]struct{}, requestedName string) ([]api.NodeConfig, error) {
	if requestedName != "" {
		for _, n := range allNodes {
			if n.Name == requestedName {
				if _, ok := existing[n.Name]; ok {
					return nil, fmt.Errorf("node %q is already a member of the cluster", requestedName)
				}
				return []api.NodeConfig{n}, nil
			}
		}
		return nil, fmt.Errorf("node %q not found in cluster.yaml", requestedName)
	}
	var out []api.NodeConfig
	for _, n := range allNodes {
		if _, ok := existing[n.Name]; !ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func pickActiveControlPlane(cfg *api.ClusterConfig, excludeNode string) (api.NodeConfig, error) {
	for _, cp := range cfg.ControlPlanes() {
		if cp.Name != excludeNode {
			return cp, nil
		}
	}
	return api.NodeConfig{}, fmt.Errorf("refusing to remove %q: cannot remove the last remaining control-plane node", excludeNode)
}

func parseUploadCertsKey(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimSpace(line)
		if len(s) == 64 && isHexString(s) {
			return s, nil
		}
	}
	return "", fmt.Errorf("failed to parse certificate key from kubeadm upload-certs output")
}

func isHexString(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

func parseEtcdMemberID(memberListOut, nodeName, nodeHost string) string {
	for _, line := range strings.Split(memberListOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 4 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[2])
		peerURL := strings.TrimSpace(parts[3])
		if name == nodeName || (nodeHost != "" && strings.Contains(peerURL, nodeHost)) {
			return id
		}
	}
	return ""
}
