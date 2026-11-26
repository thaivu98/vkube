package engine

import (
	"context"
	"fmt"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

// Status kiểm tra trạng thái các node và system pods thông qua Primary Control Plane.
func (e *Engine) Status(ctx context.Context, cfg *api.ClusterConfig) error {
	cp1 := cfg.ControlPlanes()[0]
	client, err := ssh.Dial(ctx, cp1.Name, cp1.Host, cfg.EffectiveSSH(cp1))
	if err != nil {
		return err
	}
	defer client.Close()

	fmt.Fprintf(e.Out, "==> Trạng thái cụm %q (Control Plane: %s @ %s)\n\n", cfg.Metadata.Name, cp1.Name, cp1.Host)
	nodesRes, err := client.RunSudo(ctx, "/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes -o wide")
	if err != nil {
		return fmt.Errorf("get nodes: %w", err)
	}
	fmt.Fprintf(e.Out, "[Nodes]\n%s\n", nodesRes.Stdout)

	podsRes, err := client.RunSudo(ctx, "/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system get pods -o wide")
	if err != nil {
		return fmt.Errorf("get kube-system pods: %w", err)
	}
	fmt.Fprintf(e.Out, "[kube-system Pods]\n%s", podsRes.Stdout)
	return nil
}

// Reset gỡ bỏ cụm Kubernetes song song trên toàn bộ nodes và dọn sạch cấu hình/CNI/etcd.
func (e *Engine) Reset(ctx context.Context, cfg *api.ClusterConfig) error {
	pool, err := ssh.ConnectAll(ctx, cfg, e.Out)
	if err != nil {
		return err
	}
	defer pool.Close()

	fmt.Fprintf(e.Out, "==> Đang gỡ bỏ cụm %q song song trên %d node(s)...\n", cfg.Metadata.Name, len(cfg.Spec.Nodes))
	resetScript := `set -euo pipefail
if [ -x /usr/local/bin/kubeadm ]; then
  /usr/local/bin/kubeadm reset -f --cri-socket unix:///run/containerd/containerd.sock || true
fi
systemctl stop kubelet || true
rm -rf /etc/kubernetes /var/lib/etcd /etc/cni/net.d /var/lib/cni /etc/vkube /var/lib/vkube /tmp/vkube-bundle*
iptables -F && iptables -t nat -F && iptables -t mangle -F && iptables -X || true
systemctl restart containerd || true
`
	if err := pool.RunParallel(ctx, cfg.Spec.Nodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
		if _, err := r.RunSudo(ctx, resetScript); err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "  [reset]     %-12s OK\n", node.Name)
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "Đã gỡ bỏ hoàn toàn cụm %q.\n", cfg.Metadata.Name)
	return nil
}
