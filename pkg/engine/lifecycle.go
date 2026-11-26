package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

// Status kiểm tra trạng thái các node, toàn bộ pods, StorageClasses, LoadBalancer và Metrics.
func (e *Engine) Status(ctx context.Context, cfg *api.ClusterConfig) error {
	start := time.Now()
	cp1 := cfg.ControlPlanes()[0]
	client, err := ssh.Dial(ctx, cp1.Name, cp1.Host, cfg.EffectiveSSH(cp1))
	if err != nil {
		return err
	}
	defer client.Close()

	if err := e.printClusterStatus(ctx, cfg.Metadata.Name, cp1, client); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "\nHoàn tất kiểm tra trạng thái sau %s.\n", time.Since(start).Round(time.Millisecond))
	return nil
}

// printClusterStatus dùng lại kết nối SSH có sẵn tới Primary Control Plane để in tổng quan cụm.
func (e *Engine) printClusterStatus(ctx context.Context, clusterName string, cp1 api.NodeConfig, r ssh.Runner) error {
	fmt.Fprintf(e.Out, "\n==> Trạng thái cụm %q (Control Plane: %s @ %s)\n\n", clusterName, cp1.Name, cp1.Host)
	nodesRes, err := r.RunSudo(ctx, "/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes -o wide")
	if err != nil {
		return fmt.Errorf("get nodes: %w", err)
	}
	fmt.Fprintf(e.Out, "[Nodes]\n%s\n", nodesRes.Stdout)

	podsRes, err := r.RunSudo(ctx, "/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get pods -A -o wide")
	if err != nil {
		return fmt.Errorf("get pods: %w", err)
	}
	fmt.Fprintf(e.Out, "[All Pods]\n%s\n", podsRes.Stdout)

	extraRes, err := r.RunSudo(ctx, `set -e
echo "[StorageClasses]"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get sc 2>/dev/null || true
echo ""
echo "[LoadBalancer Services]"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get svc -A --field-selector spec.type=LoadBalancer 2>/dev/null || true
echo ""
echo "[Node Metrics (kubectl top nodes)]"
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf top nodes 2>/dev/null || echo "(metrics-server đang khởi động hoặc chưa bật)"
`)
	if err == nil && extraRes.Stdout != "" {
		fmt.Fprint(e.Out, extraRes.Stdout)
	}
	return nil
}

// Reset gỡ bỏ cụm Kubernetes song song trên toàn bộ nodes và dọn sạch cấu hình/CNI/etcd.
func (e *Engine) Reset(ctx context.Context, cfg *api.ClusterConfig) error {
	start := time.Now()
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
rm -rf /etc/kubernetes /var/lib/etcd /etc/cni/net.d /var/lib/cni /etc/vkube /var/lib/vkube /tmp/vkube-bundle* /var/tmp/vkube-bundle*
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
	fmt.Fprintf(e.Out, "Đã gỡ bỏ hoàn toàn cụm %q sau %s.\n", cfg.Metadata.Name, time.Since(start).Round(time.Millisecond))
	return nil
}
