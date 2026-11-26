package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

const hostsMarkerStart = "# BEGIN vkube-managed-hosts"
const hostsMarkerEnd = "# END vkube-managed-hosts"

// RunOSPrep chuẩn bị hệ điều hành song song trên toàn bộ nodes:
// đặt hostname chuẩn, đồng bộ /etc/hosts nội bộ (cho Air-gapped), tắt swap, nạp kernel modules & sysctl.
func (e *Engine) RunOSPrep(ctx context.Context, state *ClusterState) error {
	fmt.Fprintf(e.Out, "==> [2/6] OS Prep: Chuẩn bị kernel, sysctl, swap & hostname song song...\n")
	hostsBlock := buildHostsBlock(state.Config.Spec.Nodes)

	return state.Pool.RunParallel(ctx, state.Config.Spec.Nodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
		facts := state.Facts[node.Name]
		var cmds []string
		cmds = append(cmds, "set -euo pipefail")

		// Tự động đặt lại hostname nếu chưa khớp với node.Name (tránh lỗi trùng hostname khi clone VM)
		if facts == nil || facts.Hostname != node.Name {
			cmds = append(cmds, fmt.Sprintf("hostnamectl set-hostname %s", node.Name))
		}

		// Đồng bộ /etc/hosts giữa toàn bộ node trong cụm (đảm bảo phân giải tên không cần DNS ngoài)
		cmds = append(cmds, fmt.Sprintf(`sed -i '/%s/,/%s/d' /etc/hosts
cat <<'EOF' >> /etc/hosts
%s
EOF`, hostsMarkerStart, hostsMarkerEnd, hostsBlock))

		// Tắt swap ngay lập tức và vĩnh viễn trong /etc/fstab
		cmds = append(cmds, `swapoff -a || true
sed -ri '/\sswap\s/s/^#?/#/' /etc/fstab`)

		// Nạp kernel modules cần thiết cho containerd & Calico/kube-proxy
		cmds = append(cmds, `cat <<'EOF' > /etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF
modprobe overlay
modprobe br_netfilter`)

		// Cấu hình sysctl chuẩn cho Kubernetes networking
		cmds = append(cmds, `cat <<'EOF' > /etc/sysctl.d/99-kubernetes-cri.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sysctl --system >/dev/null`)

		if _, err := r.RunSudo(ctx, strings.Join(cmds, "\n")); err != nil {
			return fmt.Errorf("os-prep failed: %w", err)
		}
		fmt.Fprintf(e.Out, "  [os-prep]   %-12s OK\n", node.Name)
		return nil
	})
}

func buildHostsBlock(nodes []api.NodeConfig) string {
	var b strings.Builder
	b.WriteString(hostsMarkerStart + "\n")
	for _, n := range nodes {
		fmt.Fprintf(&b, "%s %s\n", n.Host, n.Name)
	}
	b.WriteString(hostsMarkerEnd)
	return b.String()
}
