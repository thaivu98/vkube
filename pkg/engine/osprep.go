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
	return e.runOSPrepNodes(ctx, state, state.Config.Spec.Nodes)
}

func (e *Engine) runOSPrepNodes(ctx context.Context, state *ClusterState, targetNodes []api.NodeConfig) error {
	fmt.Fprintf(e.Out, "==> [2/6] OS Prep: Chuẩn bị kernel, sysctl, swap & hostname song song...\n")
	hostsBlock := buildHostsBlock(state.Config.Spec.Nodes)

	return state.Pool.RunParallel(ctx, targetNodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
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

		// Tối ưu riêng cho họ RHEL/Rocky/AlmaLinux 8-9: chuyển SELinux sang permissive & mở port K8s trên firewalld (nếu đang bật)
		if facts != nil && isRHELFamily(facts.OSID) {
			cmds = append(cmds, buildRHELPrepScript(node.Role))
		}

		// Nạp kernel modules cần thiết cho containerd & Calico/Cilium/kube-proxy
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

// buildRHELPrepScript cấu hình SELinux permissive và mở đúng các port Kubernetes/CNI trên firewalld (nếu đang chạy)
// cho họ Rocky Linux / AlmaLinux / RHEL 8-9 mà không tắt toàn bộ tường lửa của host.
func buildRHELPrepScript(role string) string {
	ports := []string{"10250/tcp", "30000-32767/tcp", "179/tcp", "4789/udp", "4240/tcp", "8472/udp"}
	if role == api.RoleControlPlane {
		ports = append([]string{"6443/tcp", "2379-2380/tcp", "10257/tcp", "10259/tcp"}, ports...)
	}
	var fwCmds strings.Builder
	for _, p := range ports {
		fmt.Fprintf(&fwCmds, "  firewall-cmd --permanent --add-port=%s >/dev/null 2>&1 || true\n", p)
	}
	return fmt.Sprintf(`if command -v setenforce >/dev/null 2>&1; then
  setenforce 0 2>/dev/null || true
fi
if [ -f /etc/selinux/config ]; then
  sed -i 's/^SELINUX=enforcing$/SELINUX=permissive/' /etc/selinux/config
fi
if command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
%s  firewall-cmd --permanent --add-masquerade >/dev/null 2>&1 || true
  firewall-cmd --reload >/dev/null 2>&1 || true
fi`, fwCmds.String())
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
