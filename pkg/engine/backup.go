package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

// BackupOptions chứa tham số cho lệnh `vkube backup`.
type BackupOptions struct {
	OutputPath string // Đường dẫn lưu file backup .tar.gz trên máy Deployer
}

// RestoreOptions chứa tham số cho lệnh `vkube restore`.
type RestoreOptions struct {
	BackupPath string // Đường dẫn file backup .tar.gz trên máy Deployer
}

// Backup chụp snapshot etcd (kèm verify bằng etcdutl/etcdctl snapshot status), đóng gói cùng
// toàn bộ /etc/kubernetes/pki/ và admin.conf rồi tải về máy Deployer bằng Pure Go SFTP.
func (e *Engine) Backup(ctx context.Context, cfg *api.ClusterConfig, opts BackupOptions) error {
	start := time.Now()
	cp1 := cfg.ControlPlanes()[0]

	outPath := strings.TrimSpace(opts.OutputPath)
	if outPath == "" {
		outPath = fmt.Sprintf("backup-%s-%s.tar.gz", cfg.Metadata.Name, time.Now().Format("20060102-150405"))
	}

	client, err := ssh.Dial(ctx, cp1.Name, cp1.Host, cfg.EffectiveSSH(cp1))
	if err != nil {
		return fmt.Errorf("connect primary control-plane %s: %w", cp1.Name, err)
	}
	defer client.Close()

	fmt.Fprintf(e.Out, "==> Đang sao lưu etcd & PKI của cụm %q từ %s (%s)...\n", cfg.Metadata.Name, cp1.Name, cp1.Host)

	cp1User := cfg.EffectiveSSH(cp1).User
	backupScript := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
BDIR="/var/lib/vkube/backup"
OUT_ARCHIVE="$USER_HOME/.vkube-tmp/vkube-backup.tar.gz"
rm -rf "$BDIR" && mkdir -p -m 0700 "$BDIR" "$USER_HOME/.vkube-tmp"
trap 'rm -rf "$BDIR" /var/lib/etcd/vkube-snapshot.db' EXIT

# 1. Chụp snapshot etcd ngay từ container etcd đang chạy (tương thích cả Air-gapped & Online)
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system exec etcd-%s -- \
  etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/server.crt \
  --key=/etc/kubernetes/pki/etcd/server.key \
  snapshot save /var/lib/etcd/vkube-snapshot.db

# 2. Xác thực tính toàn vẹn snapshot (gọi trực tiếp binary etcdutl/etcdctl, không phụ thuộc /bin/sh vì image etcd là distroless)
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system exec etcd-%s -- \
  etcdutl snapshot status /var/lib/etcd/vkube-snapshot.db -w table 2>/dev/null || \
/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system exec etcd-%s -- \
  etcdctl snapshot status /var/lib/etcd/vkube-snapshot.db -w table

mv /var/lib/etcd/vkube-snapshot.db "$BDIR/etcd-snapshot.db"
cp -a /etc/kubernetes/pki "$BDIR/pki"
cp -a /etc/kubernetes/admin.conf "$BDIR/admin.conf"
cat <<EOF > "$BDIR/backup-metadata.json"
{"cluster":"%s","controlPlane":"%s","createdAt":"%s"}
EOF

tar -czf "$OUT_ARCHIVE" -C "$BDIR" etcd-snapshot.db pki admin.conf backup-metadata.json
chown %s:%s "$OUT_ARCHIVE"
chmod 0600 "$OUT_ARCHIVE"
`, cp1User, cp1.Name, cp1.Name, cp1.Name, cfg.Metadata.Name, cp1.Name, time.Now().UTC().Format(time.RFC3339), cp1User, cp1User)

	res, err := client.RunSudo(ctx, backupScript)
	if err != nil {
		return fmt.Errorf("create etcd snapshot & pki backup on %s: %w", cp1.Name, err)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		fmt.Fprintf(e.Out, "  [snapshot]  Trạng thái xác thực etcd snapshot:\n%s\n", strings.TrimSpace(res.Stdout))
	}

	// Tải file backup về máy Deployer bằng Pure Go SFTP và xóa ngay file tạm trên CP-1
	fmt.Fprintf(e.Out, "  [download]  Đang tải bản sao lưu về %s...\n", outPath)
	dlErr := client.DownloadFile(ctx, ".vkube-tmp/vkube-backup.tar.gz", outPath)
	_, _ = client.RunSudo(ctx, fmt.Sprintf(`USER_HOME="$(getent passwd %s | cut -d: -f6)"; rm -f "$USER_HOME/.vkube-tmp/vkube-backup.tar.gz"`, cp1User))
	if dlErr != nil {
		return fmt.Errorf("download backup archive: %w", dlErr)
	}

	sha, size, err := fileSHA256(outPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "\nSao lưu cụm %q thành công sau %s!\n", cfg.Metadata.Name, time.Since(start).Round(time.Millisecond))
	fmt.Fprintf(e.Out, "  File:   %s (%.2f MB)\n", outPath, float64(size)/(1024*1024))
	fmt.Fprintf(e.Out, "  SHA256: %s\n", sha)
	return nil
}

// Restore khôi phục cụm 1-node hoặc 3-node HA từ file backup .tar.gz mà không bị split-brain:
// dừng kubelet/static pods đồng loạt trên toàn bộ Control Plane, khôi phục PKI + etcd với initial-cluster khớp 100%,
// sau đó khởi động lại toàn bộ Control Plane và kiểm tra trạng thái cụm.
func (e *Engine) Restore(ctx context.Context, cfg *api.ClusterConfig, opts RestoreOptions) error {
	start := time.Now()
	if strings.TrimSpace(opts.BackupPath) == "" {
		return fmt.Errorf("backup archive path is required (use --backup <file.tar.gz>)")
	}

	fmt.Fprintf(e.Out, "==> Kiểm tra tính hợp lệ của bản sao lưu %s...\n", opts.BackupPath)
	if err := verifyBackupArchive(opts.BackupPath); err != nil {
		return fmt.Errorf("invalid backup archive: %w", err)
	}

	cps := cfg.ControlPlanes()
	subCfg := *cfg
	subCfg.Spec.Nodes = cps
	pool, err := ssh.ConnectAll(ctx, &subCfg, e.Out)
	if err != nil {
		return err
	}
	defer pool.Close()

	initialCluster := buildEtcdInitialCluster(cps)
	ts := time.Now().Format("20060102-150405")
	fmt.Fprintf(e.Out, "==> Khôi phục etcd & PKI trên %d Control Plane(s) (chống split-brain: %s)...\n", len(cps), initialCluster)

	// Giai đoạn 1: Dừng toàn bộ Control Plane song song, nạp PKI & khôi phục dữ liệu etcd mới
	if err := pool.RunParallel(ctx, cps, func(ctx context.Context, cp api.NodeConfig, r ssh.Runner) error {
		remoteArchive := "/var/tmp/vkube-restore.tar.gz"
		if err := r.UploadFile(ctx, opts.BackupPath, remoteArchive); err != nil {
			return fmt.Errorf("upload backup archive: %w", err)
		}

		restoreScript := fmt.Sprintf(`set -euo pipefail
RDIR="/var/lib/vkube/restore"
rm -rf "$RDIR" && mkdir -p -m 0700 "$RDIR"
trap 'rm -rf "$RDIR" %s' EXIT

tar -xzf %s -C "$RDIR"

# 1. Dừng kubelet & tạm chuyển static pod manifests để đảm bảo không có tiến trình nào ghi vào etcd
systemctl stop kubelet || true
if [ -d /etc/kubernetes/manifests ]; then
  rm -rf /etc/kubernetes/manifests.vkube-restore-bak
  mv /etc/kubernetes/manifests /etc/kubernetes/manifests.vkube-restore-bak
fi
# Dừng các container control-plane đang chạy (nếu crictl tồn tại)
if [ -x /usr/local/bin/crictl ]; then
  for cid in $(/usr/local/bin/crictl ps -q --name 'etcd|kube-apiserver|kube-controller-manager|kube-scheduler' 2>/dev/null || true); do
    /usr/local/bin/crictl stop "$cid" >/dev/null 2>&1 || true
  done
fi

# 2. Khôi phục /etc/kubernetes/pki (giữ bản dự phòng trước khi ghi đè)
if [ -d /etc/kubernetes/pki ]; then
  mv /etc/kubernetes/pki "/etc/kubernetes/pki.bak.%s"
fi
cp -a "$RDIR/pki" /etc/kubernetes/pki
if [ -f "$RDIR/admin.conf" ]; then
  install -m 0600 "$RDIR/admin.conf" /etc/kubernetes/admin.conf
fi

# 3. Sao lưu data dir etcd cũ và khôi phục snapshot bằng đúng image etcd đã có sẵn trong containerd (100%% Air-gapped)
if [ -d /var/lib/etcd ]; then
  mv /var/lib/etcd "/var/lib/etcd.bak.%s"
fi
mkdir -p -m 0700 /var/lib/etcd

ETCD_IMG="$(/usr/local/bin/ctr -n k8s.io images ls -q | grep -E '/etcd:' | head -n 1)"
if [ -z "$ETCD_IMG" ]; then
  echo "ERROR: cannot find etcd image in containerd k8s.io namespace" >&2
  exit 1
fi

# Gọi trực tiếp binary etcdutl (etcd 3.6+) hoặc fallback etcdctl (etcd 3.5) — không dùng /bin/sh vì image etcd là distroless
if ! /usr/local/bin/ctr -n k8s.io run --rm \
  --mount type=bind,src=/var/lib/etcd,dst=/var/lib/etcd-out,options=rbind:rw \
  --mount type=bind,src="$RDIR",dst=/restore,options=rbind:ro \
  "$ETCD_IMG" "vkube-etcd-restore-%s" \
  etcdutl snapshot restore /restore/etcd-snapshot.db \
    --name %q \
    --initial-cluster %q \
    --initial-cluster-token etcd-cluster-restore \
    --initial-advertise-peer-urls "https://%s:2380" \
    --data-dir /var/lib/etcd-out/restored 2>/dev/null; then
  rm -rf /var/lib/etcd/restored
  /usr/local/bin/ctr -n k8s.io run --rm \
    --mount type=bind,src=/var/lib/etcd,dst=/var/lib/etcd-out,options=rbind:rw \
    --mount type=bind,src="$RDIR",dst=/restore,options=rbind:ro \
    "$ETCD_IMG" "vkube-etcd-restore-%s-legacy" \
    etcdctl snapshot restore /restore/etcd-snapshot.db \
      --name %q \
      --initial-cluster %q \
      --initial-cluster-token etcd-cluster-restore \
      --initial-advertise-peer-urls "https://%s:2380" \
      --data-dir /var/lib/etcd-out/restored
fi
mv /var/lib/etcd/restored/* /var/lib/etcd/
rmdir /var/lib/etcd/restored
`, remoteArchive, remoteArchive, ts, ts, cp.Name, cp.Name, initialCluster, cp.Host, cp.Name, cp.Name, initialCluster, cp.Host)

		fmt.Fprintf(e.Out, "  [restore]   %-12s đang khôi phục PKI & etcd snapshot...\n", cp.Name)
		if _, err := r.RunSudo(ctx, restoreScript); err != nil {
			return fmt.Errorf("restore etcd/pki: %w", err)
		}
		fmt.Fprintf(e.Out, "  [restore]   %-12s OK\n", cp.Name)
		return nil
	}); err != nil {
		return err
	}

	// Giai đoạn 2: Đưa static pod manifests trở lại & khởi động kubelet đồng loạt để hình thành etcd Raft quorum sạch
	fmt.Fprintf(e.Out, "==> Khởi động lại toàn bộ %d Control Plane(s)...\n", len(cps))
	if err := pool.RunParallel(ctx, cps, func(ctx context.Context, cp api.NodeConfig, r ssh.Runner) error {
		startScript := `set -euo pipefail
if [ -d /etc/kubernetes/manifests.vkube-restore-bak ]; then
  rm -rf /etc/kubernetes/manifests
  mv /etc/kubernetes/manifests.vkube-restore-bak /etc/kubernetes/manifests
fi
systemctl start kubelet`
		if _, err := r.RunSudo(ctx, startScript); err != nil {
			return fmt.Errorf("restart control-plane: %w", err)
		}
		fmt.Fprintf(e.Out, "  [start]     %-12s OK\n", cp.Name)
		return nil
	}); err != nil {
		return err
	}

	// Chờ API Server & các node Ready trở lại
	cp1 := cps[0]
	cp1Runner, err := pool.Get(cp1.Name)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  [verify]    Đang chờ Kubernetes API & các node hoạt động ổn định...\n")
	waitAPI := `set -euo pipefail
for i in $(seq 1 60); do
  if /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes >/dev/null 2>&1; then
    exit 0
  fi
  sleep 3
done
echo "ERROR: Kubernetes API did not become ready in time after restore" >&2
exit 1`
	if _, err := cp1Runner.RunSudo(ctx, waitAPI); err != nil {
		return err
	}
	_ = e.printClusterStatus(ctx, cfg.Metadata.Name, cp1, cp1Runner)

	fmt.Fprintf(e.Out, "\nKhôi phục cụm %q thành công sau %s!\n", cfg.Metadata.Name, time.Since(start).Round(time.Second))
	return nil
}

// buildEtcdInitialCluster tạo chuỗi --initial-cluster chuẩn cho etcd Raft restore (ví dụ cp-1=https://10.0.0.1:2380,cp-2=https://10.0.0.2:2380).
func buildEtcdInitialCluster(cps []api.NodeConfig) string {
	peers := make([]string, 0, len(cps))
	for _, cp := range cps {
		peers = append(peers, fmt.Sprintf("%s=https://%s:2380", cp.Name, cp.Host))
	}
	return strings.Join(peers, ",")
}

// verifyBackupArchive kiểm tra file .tar.gz bằng Pure Go đảm bảo chứa đủ etcd-snapshot.db và thư mục pki/.
func verifyBackupArchive(localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	var hasSnapshot, hasPKI bool
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		clean := strings.TrimPrefix(hdr.Name, "./")
		if clean == "etcd-snapshot.db" && hdr.Size > 0 {
			hasSnapshot = true
		}
		if strings.HasPrefix(clean, "pki/") && hdr.Typeflag == tar.TypeReg {
			hasPKI = true
		}
	}
	if !hasSnapshot {
		return fmt.Errorf("archive missing non-empty etcd-snapshot.db")
	}
	if !hasPKI {
		return fmt.Errorf("archive missing pki/ certificates")
	}
	return nil
}

func fileSHA256(localPath string) (string, int64, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
