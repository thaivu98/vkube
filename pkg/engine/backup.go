package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

// BackupOptions chứa tham số cho lệnh `vkube backup`.
type BackupOptions struct {
	OutputPath string // Đường dẫn lưu file backup cục bộ (nếu trống: lưu theo spec.backup.local.dir hoặc file tạm khi đẩy S3/SFTP)
	Target     string // Ghi đè đích lưu trữ: local | s3 | sftp (bỏ trống sẽ lấy từ spec.backup.target)
}

// RestoreOptions chứa tham số cho lệnh `vkube restore`.
type RestoreOptions struct {
	BackupPath           string // Đường dẫn file backup .tar.gz trên máy Deployer hoặc URI s3:// / sftp://
	ExpectedSHA256       string // Tùy chọn: xác thực SHA256 của file backup trước khi khôi phục
	ForceClusterMismatch bool   // Cho phép khôi phục ngay cả khi tên cụm trong backup-metadata.json khác cluster.yaml
}

// Backup chụp snapshot etcd (kèm verify bằng etcdutl/etcdctl snapshot status), đóng gói cùng
// toàn bộ /etc/kubernetes/pki/ và admin.conf rồi lưu về đích đã cấu hình (local, S3/MinIO, hoặc SFTP/NAS).
func (e *Engine) Backup(ctx context.Context, cfg *api.ClusterConfig, opts BackupOptions) error {
	start := time.Now()
	cp1 := cfg.ControlPlanes()[0]

	target := strings.TrimSpace(opts.Target)
	if target == "" {
		target = cfg.Spec.Backup.Target
	}
	if target == "" {
		target = api.BackupTargetLocal
	}

	defaultFileName := fmt.Sprintf("backup-%s-%s.tar.gz", cfg.Metadata.Name, time.Now().Format("20060102-150405"))
	localPath := strings.TrimSpace(opts.OutputPath)
	removeLocalAfterUpload := false

	switch target {
	case api.BackupTargetLocal:
		if localPath == "" {
			dir := ""
			if cfg.Spec.Backup.Local != nil {
				dir = strings.TrimSpace(cfg.Spec.Backup.Local.Dir)
			}
			if dir != "" {
				localPath = filepath.Join(dir, defaultFileName)
			} else {
				localPath = defaultFileName
			}
		}
	case api.BackupTargetS3:
		if cfg.Spec.Backup.S3 == nil {
			return fmt.Errorf("backup target is \"s3\" but spec.backup.s3 is not configured")
		}
		if localPath == "" {
			localPath = filepath.Join(os.TempDir(), defaultFileName)
			removeLocalAfterUpload = true
		}
	case api.BackupTargetSFTP:
		if cfg.Spec.Backup.SFTP == nil {
			return fmt.Errorf("backup target is \"sftp\" but spec.backup.sftp is not configured")
		}
		if localPath == "" {
			localPath = filepath.Join(os.TempDir(), defaultFileName)
			removeLocalAfterUpload = true
		}
	default:
		return fmt.Errorf("unsupported backup target %q (supported: local, s3, sftp)", target)
	}
	if removeLocalAfterUpload {
		defer os.Remove(localPath)
	}

	client, err := ssh.Dial(ctx, cp1.Name, cp1.Host, cfg.EffectiveSSH(cp1))
	if err != nil {
		return fmt.Errorf("connect primary control-plane %s: %w", cp1.Name, err)
	}
	defer client.Close()

	fmt.Fprintf(e.Out, "==> Đang sao lưu etcd & PKI của cụm %q từ %s (%s) [target: %s]...\n", cfg.Metadata.Name, cp1.Name, cp1.Host, target)

	cp1User := cfg.EffectiveSSH(cp1).User
	backupScript := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
BDIR="/var/lib/vkube/backup"
OUT_ARCHIVE="$USER_HOME/.vkube-tmp/vkube-backup.tar.gz"
rm -rf "$BDIR" && mkdir -p -m 0700 "$BDIR" "$USER_HOME/.vkube-tmp"
trap 'rm -rf "$BDIR" /var/lib/etcd/vkube-snapshot.db' EXIT

# 1. Chụp snapshot etcd trực tiếp qua crictl exec (không phụ thuộc kube-apiserver hay kubelet-serving TLS, chạy tốt ngay cả khi cụm đang sự cố)
ETCD_CID="$(/usr/local/bin/crictl ps --name '^etcd$' -q | head -n 1)"
if [ -z "$ETCD_CID" ]; then
  echo "ERROR: cannot find running etcd container via crictl" >&2
  exit 1
fi
/usr/local/bin/crictl exec "$ETCD_CID" \
  etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/server.crt \
  --key=/etc/kubernetes/pki/etcd/server.key \
  snapshot save /var/lib/etcd/vkube-snapshot.db

# 2. Xác thực tính toàn vẹn snapshot (gọi trực tiếp binary etcdutl/etcdctl, không phụ thuộc /bin/sh vì image etcd là distroless)
/usr/local/bin/crictl exec "$ETCD_CID" \
  etcdutl snapshot status /var/lib/etcd/vkube-snapshot.db -w table 2>/dev/null || \
/usr/local/bin/crictl exec "$ETCD_CID" \
  etcdctl snapshot status /var/lib/etcd/vkube-snapshot.db -w table

mv /var/lib/etcd/vkube-snapshot.db "$BDIR/etcd-snapshot.db"
cp -a /etc/kubernetes/pki "$BDIR/pki"
cp -a /etc/kubernetes/admin.conf "$BDIR/admin.conf"
EXTRA_DIRS=""
if [ -d /etc/kubernetes/enc ]; then
  cp -a /etc/kubernetes/enc "$BDIR/enc"
  EXTRA_DIRS="enc"
fi
cat <<EOF > "$BDIR/backup-metadata.json"
{"cluster":"%s","controlPlane":"%s","createdAt":"%s"}
EOF

tar -czf "$OUT_ARCHIVE" -C "$BDIR" etcd-snapshot.db pki admin.conf backup-metadata.json $EXTRA_DIRS
chown %s:%s "$OUT_ARCHIVE"
chmod 0600 "$OUT_ARCHIVE"
`, cp1User, cfg.Metadata.Name, cp1.Name, time.Now().UTC().Format(time.RFC3339), cp1User, cp1User)

	res, err := client.RunSudo(ctx, backupScript)
	if err != nil {
		return fmt.Errorf("create etcd snapshot & pki backup on %s: %w", cp1.Name, err)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		fmt.Fprintf(e.Out, "  [snapshot]  Trạng thái xác thực etcd snapshot:\n%s\n", strings.TrimSpace(res.Stdout))
	}

	// Tải file backup về máy Deployer bằng Pure Go SFTP và xóa ngay file tạm trên CP-1
	fmt.Fprintf(e.Out, "  [download]  Đang kéo bản sao lưu từ %s về máy Deployer...\n", cp1.Name)
	dlErr := client.DownloadFile(ctx, ".vkube-tmp/vkube-backup.tar.gz", localPath)
	_, _ = client.RunSudo(ctx, fmt.Sprintf(`USER_HOME="$(getent passwd %s | cut -d: -f6)"; rm -f "$USER_HOME/.vkube-tmp/vkube-backup.tar.gz"`, cp1User))
	if dlErr != nil {
		return fmt.Errorf("download backup archive: %w", dlErr)
	}

	// Mã hóa xác thực AES-256-GCM phía client nếu có mật khẩu (bảo vệ CA private key & etcd Secrets khi lưu lên S3/SFTP)
	if pass := cfg.Spec.Backup.EncryptionPassphrase; pass != "" {
		encTmp := localPath + ".enc.tmp"
		if err := encryptBackupFile(localPath, encTmp, pass); err != nil {
			_ = os.Remove(encTmp)
			return fmt.Errorf("encrypt backup archive: %w", err)
		}
		if err := os.Rename(encTmp, localPath); err != nil {
			_ = os.Remove(encTmp)
			return err
		}
		fmt.Fprintf(e.Out, "  [encrypt]   Đã mã hóa xác thực bản sao lưu bằng AES-256-GCM (PBKDF2-SHA256)\n")
	} else if target == api.BackupTargetS3 || target == api.BackupTargetSFTP {
		fmt.Fprintf(e.Out, "  [WARN]      Bản sao lưu chứa CA private key & etcd Secrets đang được đẩy lên %s KHÔNG mã hóa! Khuyến nghị đặt biến môi trường VKUBE_BACKUP_PASSPHRASE.\n", strings.ToUpper(target))
	}

	sha, size, err := fileSHA256(localPath)
	if err != nil {
		return err
	}

	// Đẩy lên đích lưu trữ từ xa (S3/MinIO hoặc SFTP/NAS) nếu được chọn
	fileName := filepath.Base(localPath)
	var remoteURI string
	switch target {
	case api.BackupTargetS3:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(cfg.Spec.Backup.S3.Endpoint)), "http://") {
			fmt.Fprintf(e.Out, "  [WARN]      Endpoint S3 đang dùng HTTP thường (%s) — khuyến nghị chuyển sang HTTPS.\n", cfg.Spec.Backup.S3.Endpoint)
		}
		objectKey := fileName
		if prefix := strings.Trim(cfg.Spec.Backup.S3.Prefix, "/"); prefix != "" {
			objectKey = path.Join(prefix, fileName)
		}
		fmt.Fprintf(e.Out, "  [upload]    Đang đẩy bản sao lưu lên S3/MinIO (%s -> bucket %s)...\n", cfg.Spec.Backup.S3.Endpoint, cfg.Spec.Backup.S3.Bucket)
		remoteURI, err = s3UploadFile(ctx, cfg.Spec.Backup.S3, localPath, objectKey)
		if err != nil {
			return err
		}
	case api.BackupTargetSFTP:
		fmt.Fprintf(e.Out, "  [upload]    Đang đẩy bản sao lưu lên SFTP (%s@%s:%s)...\n", cfg.Spec.Backup.SFTP.User, cfg.Spec.Backup.SFTP.Host, cfg.Spec.Backup.SFTP.RemoteDir)
		remoteURI, err = sftpUploadBackup(ctx, cfg.Spec.Backup.SFTP, localPath, fileName)
		if err != nil {
			return err
		}
	}

	// Dọn các bản sao lưu cũ theo chính sách retention (nếu spec.backup.retention > 0)
	if ret := cfg.Spec.Backup.Retention; ret > 0 {
		filePrefix := fmt.Sprintf("backup-%s-", cfg.Metadata.Name)
		var pruned []string
		switch target {
		case api.BackupTargetLocal:
			pruned, err = pruneLocalBackups(filepath.Dir(localPath), filePrefix, ret)
		case api.BackupTargetS3:
			s3Prefix := filePrefix
			if p := strings.Trim(cfg.Spec.Backup.S3.Prefix, "/"); p != "" {
				s3Prefix = path.Join(p, filePrefix)
			}
			pruned, err = s3PruneBackups(ctx, cfg.Spec.Backup.S3, s3Prefix, ret)
		case api.BackupTargetSFTP:
			pruned, err = sftpPruneBackups(ctx, cfg.Spec.Backup.SFTP, filePrefix, ret)
		}
		if err != nil {
			fmt.Fprintf(e.Out, "  [retention] Cảnh báo: không thể dọn bản sao lưu cũ (%v)\n", err)
		} else if len(pruned) > 0 {
			fmt.Fprintf(e.Out, "  [retention] Đã tự động xóa %d bản sao lưu cũ (giữ %d bản mới nhất)\n", len(pruned), ret)
		}
	}

	fmt.Fprintf(e.Out, "\nSao lưu cụm %q thành công sau %s!\n", cfg.Metadata.Name, time.Since(start).Round(time.Millisecond))
	if remoteURI != "" {
		fmt.Fprintf(e.Out, "  Remote: %s (%.2f MB)\n", remoteURI, float64(size)/(1024*1024))
		if !removeLocalAfterUpload {
			fmt.Fprintf(e.Out, "  Local:  %s\n", localPath)
		}
	} else {
		fmt.Fprintf(e.Out, "  File:   %s (%.2f MB)\n", localPath, float64(size)/(1024*1024))
	}
	fmt.Fprintf(e.Out, "  SHA256: %s\n", sha)
	return nil
}

// Restore khôi phục cụm 1-node hoặc 3-node HA từ file backup .tar.gz (hỗ trợ đường dẫn cục bộ, s3://..., hoặc sftp://...)
// mà không bị split-brain: dừng kubelet/static pods đồng loạt trên toàn bộ Control Plane, khôi phục PKI + etcd với initial-cluster khớp 100%,
// sau đó khởi động lại toàn bộ Control Plane và kiểm tra trạng thái cụm.
func (e *Engine) Restore(ctx context.Context, cfg *api.ClusterConfig, opts RestoreOptions) error {
	start := time.Now()
	backupRef := strings.TrimSpace(opts.BackupPath)
	if backupRef == "" {
		return fmt.Errorf("backup archive path or URI is required (use --backup <file.tar.gz | s3://... | sftp://...>)")
	}

	localBackup := backupRef
	if bucket, key, ok := parseS3URI(backupRef); ok {
		if cfg.Spec.Backup.S3 == nil {
			return fmt.Errorf("restore from %q requires spec.backup.s3 (or --s3-* flags)", backupRef)
		}
		s3Cfg := *cfg.Spec.Backup.S3
		s3Cfg.Bucket = bucket
		tmpFile, err := os.CreateTemp("", "vkube-restore-*.tar.gz")
		if err != nil {
			return err
		}
		localBackup = tmpFile.Name()
		_ = tmpFile.Close()
		defer os.Remove(localBackup)
		fmt.Fprintf(e.Out, "==> Đang tải bản sao lưu từ S3 (%s)...\n", backupRef)
		if err := s3DownloadFile(ctx, &s3Cfg, key, localBackup); err != nil {
			return err
		}
	} else if remotePath, ok := parseSFTPURI(backupRef); ok {
		if cfg.Spec.Backup.SFTP == nil {
			return fmt.Errorf("restore from %q requires spec.backup.sftp (or --sftp-* flags)", backupRef)
		}
		tmpFile, err := os.CreateTemp("", "vkube-restore-*.tar.gz")
		if err != nil {
			return err
		}
		localBackup = tmpFile.Name()
		_ = tmpFile.Close()
		defer os.Remove(localBackup)
		fmt.Fprintf(e.Out, "==> Đang tải bản sao lưu từ SFTP (%s)...\n", backupRef)
		if err := sftpDownloadBackup(ctx, cfg.Spec.Backup.SFTP, remotePath, localBackup); err != nil {
			return err
		}
	}

	// Xác thực SHA256 nếu người dùng truyền --sha256 (chống archive bị sửa đổi trên S3/SFTP)
	if wantSHA := strings.ToLower(strings.TrimSpace(opts.ExpectedSHA256)); wantSHA != "" {
		gotSHA, _, err := fileSHA256(localBackup)
		if err != nil {
			return fmt.Errorf("hash backup archive: %w", err)
		}
		if gotSHA != wantSHA {
			return fmt.Errorf("backup SHA256 mismatch: expected %s, got %s", wantSHA, gotSHA)
		}
		fmt.Fprintf(e.Out, "  [verify]    SHA256 bản sao lưu khớp chính xác (%s)\n", gotSHA)
	}

	// Tự động giải mã AES-256-GCM nếu archive đã được mã hóa bởi `vkube backup`
	if isEncryptedBackup(localBackup) {
		fmt.Fprintf(e.Out, "  [decrypt]   Phát hiện bản sao lưu được mã hóa AES-256-GCM — đang giải mã & xác thực...\n")
		decFile, err := os.CreateTemp("", "vkube-restore-dec-*.tar.gz")
		if err != nil {
			return err
		}
		decPath := decFile.Name()
		_ = decFile.Close()
		defer os.Remove(decPath)
		if err := decryptBackupFile(localBackup, decPath, cfg.Spec.Backup.EncryptionPassphrase); err != nil {
			return err
		}
		localBackup = decPath
	}

	fmt.Fprintf(e.Out, "==> Kiểm tra tính hợp lệ & cluster identity của bản sao lưu %s...\n", backupRef)
	if err := verifyBackupArchive(localBackup, cfg.Metadata.Name, opts.ForceClusterMismatch); err != nil {
		return fmt.Errorf("invalid backup archive: %w", err)
	}
	opts.BackupPath = localBackup

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

	// Giai đoạn 1: Dừng toàn bộ Control Plane song song, nạp PKI + enc/ & khôi phục dữ liệu etcd mới
	if err := pool.RunParallel(ctx, cps, func(ctx context.Context, cp api.NodeConfig, r ssh.Runner) error {
		archiveBytes, err := os.ReadFile(opts.BackupPath)
		if err != nil {
			return fmt.Errorf("read backup archive: %w", err)
		}
		// Ghi vào thư mục riêng .vkube-tmp với quyền 0600 (KHÔNG dùng /var/tmp 0644 vì chứa CA private key)
		if err := r.WriteFile(ctx, archiveBytes, ".vkube-tmp/vkube-restore.tar.gz", 0o600); err != nil {
			return fmt.Errorf("upload backup archive: %w", err)
		}
		cpUser := cfg.EffectiveSSH(cp).User
		cpNodeIP := cp.EffectiveNodeIP()

		restoreScript := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
ARCHIVE="$USER_HOME/.vkube-tmp/vkube-restore.tar.gz"
RDIR="/var/lib/vkube/restore"
rm -rf "$RDIR" && mkdir -p -m 0700 "$RDIR"
trap 'rm -rf "$RDIR" "$ARCHIVE"' EXIT

tar -xzf "$ARCHIVE" -C "$RDIR"

# 1. Dừng kubelet & tạm chuyển static pod manifests để đảm bảo không có tiến trình nào ghi vào etcd
systemctl stop kubelet || true
if [ -d /etc/kubernetes/manifests ]; then
  rm -rf /etc/kubernetes/manifests.vkube-restore-bak
  mv /etc/kubernetes/manifests /etc/kubernetes/manifests.vkube-restore-bak
fi
if [ -x /usr/local/bin/crictl ]; then
  for cid in $(/usr/local/bin/crictl ps -q --name 'etcd|kube-apiserver|kube-controller-manager|kube-scheduler' 2>/dev/null || true); do
    /usr/local/bin/crictl stop "$cid" >/dev/null 2>&1 || true
  done
fi

# 2. Khôi phục /etc/kubernetes/pki & /etc/kubernetes/enc (nếu có khóa Encryption at Rest)
if [ -d /etc/kubernetes/pki ]; then
  mv /etc/kubernetes/pki "/etc/kubernetes/pki.bak.%s"
fi
cp -a "$RDIR/pki" /etc/kubernetes/pki
if [ -d "$RDIR/enc" ]; then
  if [ -d /etc/kubernetes/enc ]; then
    mv /etc/kubernetes/enc "/etc/kubernetes/enc.bak.%s"
  fi
  cp -a "$RDIR/enc" /etc/kubernetes/enc
  chmod 0700 /etc/kubernetes/enc
  chmod 0600 /etc/kubernetes/enc/* 2>/dev/null || true
fi
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
`, cpUser, ts, ts, ts, cp.Name, cp.Name, initialCluster, cpNodeIP, cp.Name, cp.Name, initialCluster, cpNodeIP)

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
		peers = append(peers, fmt.Sprintf("%s=https://%s:2380", cp.Name, cp.EffectiveNodeIP()))
	}
	return strings.Join(peers, ",")
}

type backupMetadata struct {
	Cluster      string `json:"cluster"`
	ControlPlane string `json:"controlPlane"`
	CreatedAt    string `json:"createdAt"`
}

// verifyBackupArchive kiểm tra file .tar.gz bằng Pure Go đảm bảo chứa đủ etcd-snapshot.db, thư mục pki/,
// và đối chiếu cluster identity trong backup-metadata.json để chống khôi phục nhầm snapshot của cụm khác.
func verifyBackupArchive(localPath, expectedCluster string, forceClusterMismatch bool) error {
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
	var (
		hasSnapshot bool
		hasPKI      bool
		meta        backupMetadata
		hasMeta     bool
	)
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
		if clean == "backup-metadata.json" && hdr.Size > 0 && hdr.Size < 64*1024 {
			if raw, rerr := io.ReadAll(tr); rerr == nil {
				if jerr := json.Unmarshal(raw, &meta); jerr == nil {
					hasMeta = true
				}
			}
		}
	}
	if !hasSnapshot {
		return fmt.Errorf("archive missing non-empty etcd-snapshot.db")
	}
	if !hasPKI {
		return fmt.Errorf("archive missing pki/ certificates")
	}
	if hasMeta && expectedCluster != "" && meta.Cluster != "" && meta.Cluster != expectedCluster && !forceClusterMismatch {
		return fmt.Errorf("backup belongs to cluster %q, but target cluster.yaml is %q (pass --force-cluster-mismatch to override)", meta.Cluster, expectedCluster)
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

// InstallBackupSchedule cài đặt systemd service + timer (vkube-etcd-backup.timer) trên Primary Control Plane
// để tự động chụp snapshot etcd + PKI định kỳ vào /var/backups/vkube-etcd và tự xoay vòng theo retention.
func (e *Engine) InstallBackupSchedule(ctx context.Context, cfg *api.ClusterConfig, onCalendar string) error {
	cp1 := cfg.ControlPlanes()[0]
	if strings.TrimSpace(onCalendar) == "" {
		onCalendar = strings.TrimSpace(cfg.Spec.Backup.Schedule)
	}
	if onCalendar == "" {
		onCalendar = "daily"
	}
	retention := cfg.Spec.Backup.Retention
	if retention <= 0 {
		retention = 7
	}

	client, err := ssh.Dial(ctx, cp1.Name, cp1.Host, cfg.EffectiveSSH(cp1))
	if err != nil {
		return fmt.Errorf("connect primary control-plane %s: %w", cp1.Name, err)
	}
	defer client.Close()

	script := fmt.Sprintf(`set -euo pipefail
mkdir -p -m 0700 /var/backups/vkube-etcd
cat <<'EOF' > /usr/local/bin/vkube-etcd-backup.sh
#!/usr/bin/env bash
set -euo pipefail
TS="$(date +%%Y%%m%%d-%%H%%M%%S)"
BDIR="/var/lib/vkube/sched-backup"
OUT="/var/backups/vkube-etcd/backup-%s-${TS}.tar.gz"
rm -rf "$BDIR" && mkdir -p -m 0700 "$BDIR"
trap 'rm -rf "$BDIR" /var/lib/etcd/vkube-sched-snapshot.db' EXIT
ETCD_CID="$(/usr/local/bin/crictl ps --name '^etcd$' -q | head -n 1)"
/usr/local/bin/crictl exec "$ETCD_CID" \
  etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/server.crt \
  --key=/etc/kubernetes/pki/etcd/server.key \
  snapshot save /var/lib/etcd/vkube-sched-snapshot.db
mv /var/lib/etcd/vkube-sched-snapshot.db "$BDIR/etcd-snapshot.db"
cp -a /etc/kubernetes/pki "$BDIR/pki"
cp -a /etc/kubernetes/admin.conf "$BDIR/admin.conf"
tar -czf "$OUT" -C "$BDIR" etcd-snapshot.db pki admin.conf
chmod 0600 "$OUT"
ls -1t /var/backups/vkube-etcd/backup-%s-*.tar.gz 2>/dev/null | tail -n +%d | xargs -r rm -f
EOF
chmod 0700 /usr/local/bin/vkube-etcd-backup.sh

cat <<'EOF' > /etc/systemd/system/vkube-etcd-backup.service
[Unit]
Description=vkube scheduled etcd & PKI backup

[Service]
Type=oneshot
ExecStart=/usr/local/bin/vkube-etcd-backup.sh
EOF

cat <<'EOF' > /etc/systemd/system/vkube-etcd-backup.timer
[Unit]
Description=vkube scheduled etcd & PKI backup timer

[Timer]
OnCalendar=%s
Persistent=true

[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
systemctl enable --now vkube-etcd-backup.timer
systemctl list-timers --no-pager vkube-etcd-backup.timer || true
`, cfg.Metadata.Name, cfg.Metadata.Name, retention+1, onCalendar)

	res, err := client.RunSudo(ctx, script)
	if err != nil {
		return fmt.Errorf("install backup systemd timer on %s: %w", cp1.Name, err)
	}
	fmt.Fprintf(e.Out, "Đã cài đặt lịch backup etcd tự động trên %s (OnCalendar=%q, retention=%d, thư mục /var/backups/vkube-etcd)!\n%s\n",
		cp1.Name, onCalendar, retention, strings.TrimSpace(res.Stdout))
	return nil
}
