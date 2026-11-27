package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

// CertsCheck kiểm tra thời hạn toàn bộ chứng chỉ Kubernetes (kubeadm certs check-expiration) trên tất cả Control Plane.
func (e *Engine) CertsCheck(ctx context.Context, cfg *api.ClusterConfig) error {
	cps := cfg.ControlPlanes()
	subCfg := *cfg
	subCfg.Spec.Nodes = cps
	pool, err := ssh.ConnectAll(ctx, &subCfg, e.Out)
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, cp := range cps {
		r, err := pool.Get(cp.Name)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "\n==> Kiểm tra hạn chứng chỉ trên Control Plane %s (%s):\n", cp.Name, cp.Host)
		res, err := r.RunSudo(ctx, "/usr/local/bin/kubeadm certs check-expiration")
		if err != nil {
			return fmt.Errorf("check cert expiration on %s: %w", cp.Name, err)
		}
		fmt.Fprintln(e.Out, res.Stdout)
	}
	return nil
}

// CertsRenew gia hạn toàn bộ chứng chỉ Kubernetes tuần tự trên từng Control Plane (giữ HA API không gián đoạn),
// khởi động lại các static pod control-plane, cập nhật ~/.kube/config trên CP-1 và xuất lại kubeconfig cục bộ.
func (e *Engine) CertsRenew(ctx context.Context, cfg *api.ClusterConfig, kubeconfigOut string) error {
	start := time.Now()
	cps := cfg.ControlPlanes()
	subCfg := *cfg
	subCfg.Spec.Nodes = cps
	pool, err := ssh.ConnectAll(ctx, &subCfg, e.Out)
	if err != nil {
		return err
	}
	defer pool.Close()

	fmt.Fprintf(e.Out, "==> Đang gia hạn toàn bộ chứng chỉ Kubernetes tuần tự trên %d Control Plane(s)...\n", len(cps))
	for _, cp := range cps {
		r, err := pool.Get(cp.Name)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "  [certs]     %-12s gia hạn chứng chỉ & khởi động lại static pods...\n", cp.Name)
		renewScript := `set -euo pipefail
/usr/local/bin/kubeadm certs renew all
# Dừng nhẹ các container static pod để kubelet tự bật lại với bộ chứng chỉ mới
if [ -x /usr/local/bin/crictl ]; then
  for cid in $(/usr/local/bin/crictl ps -q --name 'kube-apiserver|kube-controller-manager|kube-scheduler|etcd' 2>/dev/null || true); do
    /usr/local/bin/crictl stop "$cid" >/dev/null 2>&1 || true
  done
fi
for i in $(seq 1 30); do
  if /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get --raw='/readyz' >/dev/null 2>&1; then
    exit 0
  fi
  sleep 2
done
echo "ERROR: kube-apiserver did not recover in time after cert renewal" >&2
exit 1`
		if _, err := r.RunSudo(ctx, renewScript); err != nil {
			return fmt.Errorf("renew certs on %s: %w", cp.Name, err)
		}
		fmt.Fprintf(e.Out, "  [certs]     %-12s OK\n", cp.Name)
	}

	// Cập nhật lại ~/.kube/config trên CP-1 và kéo kubeconfig mới về máy Deployer
	cp1 := cps[0]
	cp1Runner, err := pool.Get(cp1.Name)
	if err != nil {
		return err
	}
	cp1User := cfg.EffectiveSSH(cp1).User
	_, _ = cp1Runner.RunSudo(ctx, fmt.Sprintf(`set -euo pipefail
HOME_DIR="$(getent passwd %s | cut -d: -f6)"
if [ -n "$HOME_DIR" ]; then
  mkdir -p "$HOME_DIR/.kube"
  cp -f /etc/kubernetes/admin.conf "$HOME_DIR/.kube/config"
  chown -R %s:%s "$HOME_DIR/.kube"
  chmod 0600 "$HOME_DIR/.kube/config"
fi`, cp1User, cp1User, cp1User))

	res, err := cp1Runner.RunSudo(ctx, "cat /etc/kubernetes/admin.conf")
	if err != nil {
		return fmt.Errorf("fetch renewed admin.conf: %w", err)
	}
	kubeconfigContent := strings.ReplaceAll(res.Stdout,
		fmt.Sprintf("kubernetes-admin@%s", cfg.Metadata.Name),
		cfg.Metadata.Name,
	)
	if kubeconfigOut == "" {
		kubeconfigOut = fmt.Sprintf("kubeconfig-%s.yaml", cfg.Metadata.Name)
	}
	if dir := filepath.Dir(kubeconfigOut); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(kubeconfigOut, []byte(kubeconfigContent), 0o600); err != nil {
		return fmt.Errorf("write renewed kubeconfig %q: %w", kubeconfigOut, err)
	}
	fmt.Fprintf(e.Out, "  [kubeconfig] Đã cập nhật kubeconfig mới tại: %s\n", kubeconfigOut)
	fmt.Fprintf(e.Out, "\nĐã gia hạn toàn bộ chứng chỉ cụm %q thành công sau %s!\n", cfg.Metadata.Name, time.Since(start).Round(time.Second))
	return nil
}
