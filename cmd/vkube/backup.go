package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/engine"
)

type backupStorageFlags struct {
	target       string
	s3Endpoint   string
	s3Bucket     string
	s3Prefix     string
	s3Region     string
	s3AccessKey  string
	s3SecretKey  string
	s3Insecure   bool
	sftpHost     string
	sftpPort     int
	sftpUser     string
	sftpKeyPath  string
	sftpPassword string
	sftpDir      string
}

func (f *backupStorageFlags) bind(cmd *cobra.Command, includeTarget bool) {
	if includeTarget {
		cmd.Flags().StringVar(&f.target, "target", "", "Đích lưu trữ backup: local | s3 | sftp (ghi đè spec.backup.target)")
	}
	cmd.Flags().StringVar(&f.s3Endpoint, "s3-endpoint", "", "Endpoint S3 / MinIO / Ceph RGW (vd: https://minio.internal:9000)")
	cmd.Flags().StringVar(&f.s3Bucket, "s3-bucket", "", "Tên bucket S3 lưu bản sao lưu")
	cmd.Flags().StringVar(&f.s3Prefix, "s3-prefix", "", "Thư mục con (key prefix) trong bucket S3")
	cmd.Flags().StringVar(&f.s3Region, "s3-region", "", "AWS Region cho S3 (mặc định: us-east-1)")
	cmd.Flags().StringVar(&f.s3AccessKey, "s3-access-key", "", "S3 Access Key (hoặc biến môi trường VKUBE_S3_ACCESS_KEY)")
	cmd.Flags().StringVar(&f.s3SecretKey, "s3-secret-key", "", "S3 Secret Key (hoặc biến môi trường VKUBE_S3_SECRET_KEY)")
	cmd.Flags().BoolVar(&f.s3Insecure, "s3-insecure", false, "Bỏ qua kiểm tra TLS cho MinIO/Ceph self-signed nội bộ")
	cmd.Flags().StringVar(&f.sftpHost, "sftp-host", "", "Địa chỉ IP/Host của Backup Server / NAS (SFTP)")
	cmd.Flags().IntVar(&f.sftpPort, "sftp-port", 0, "Cổng SSH/SFTP của Backup Server (mặc định: 22)")
	cmd.Flags().StringVar(&f.sftpUser, "sftp-user", "", "User đăng nhập SFTP Backup Server")
	cmd.Flags().StringVar(&f.sftpKeyPath, "sftp-key", "", "Đường dẫn SSH private key cho SFTP Backup Server")
	cmd.Flags().StringVar(&f.sftpPassword, "sftp-password", "", "Mật khẩu SFTP Backup Server (hoặc env VKUBE_BACKUP_SFTP_PASSWORD)")
	cmd.Flags().StringVar(&f.sftpDir, "sftp-dir", "", "Thư mục đích trên SFTP Backup Server (vd: /data/backups/k8s)")
}

func (f *backupStorageFlags) applyTo(cfg *api.ClusterConfig, insecureHostKey bool) error {
	if f.target != "" {
		cfg.Spec.Backup.Target = f.target
	}
	if f.s3Endpoint != "" || f.s3Bucket != "" || f.s3Prefix != "" || f.s3Region != "" || f.s3AccessKey != "" || f.s3SecretKey != "" || f.s3Insecure {
		if cfg.Spec.Backup.S3 == nil {
			cfg.Spec.Backup.S3 = &api.S3BackupConfig{}
		}
		if f.s3Endpoint != "" {
			cfg.Spec.Backup.S3.Endpoint = f.s3Endpoint
		}
		if f.s3Bucket != "" {
			cfg.Spec.Backup.S3.Bucket = f.s3Bucket
		}
		if f.s3Prefix != "" {
			cfg.Spec.Backup.S3.Prefix = f.s3Prefix
		}
		if f.s3Region != "" {
			cfg.Spec.Backup.S3.Region = f.s3Region
		}
		if f.s3AccessKey != "" {
			cfg.Spec.Backup.S3.AccessKey = f.s3AccessKey
		}
		if f.s3SecretKey != "" {
			cfg.Spec.Backup.S3.SecretKey = f.s3SecretKey
		}
		if f.s3Insecure {
			cfg.Spec.Backup.S3.InsecureSkipVerify = true
		}
	}
	if f.sftpHost != "" || f.sftpPort != 0 || f.sftpUser != "" || f.sftpKeyPath != "" || f.sftpPassword != "" || f.sftpDir != "" {
		if cfg.Spec.Backup.SFTP == nil {
			cfg.Spec.Backup.SFTP = &api.SFTPBackupConfig{}
		}
		if f.sftpHost != "" {
			cfg.Spec.Backup.SFTP.Host = f.sftpHost
		}
		if f.sftpPort != 0 {
			cfg.Spec.Backup.SFTP.Port = f.sftpPort
		}
		if f.sftpUser != "" {
			cfg.Spec.Backup.SFTP.User = f.sftpUser
		}
		if f.sftpKeyPath != "" {
			cfg.Spec.Backup.SFTP.PrivateKeyPath = f.sftpKeyPath
		}
		if f.sftpPassword != "" {
			cfg.Spec.Backup.SFTP.Password = f.sftpPassword
		}
		if f.sftpDir != "" {
			cfg.Spec.Backup.SFTP.RemoteDir = f.sftpDir
		}
	}
	if insecureHostKey && cfg.Spec.Backup.SFTP != nil {
		cfg.Spec.Backup.SFTP.InsecureIgnoreHostKey = true
	}
	cfg.ApplyDefaults()
	return cfg.Validate()
}

func newBackupCmd() *cobra.Command {
	var (
		configFile      string
		outputPath      string
		retention       int
		insecureHostKey bool
		storageFlags    backupStorageFlags
	)

	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Chụp snapshot etcd (có verify) & sao lưu PKI về Local, S3/MinIO hoặc SFTP/NAS",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			cfg, err := api.LoadClusterConfig(configFile)
			if err != nil {
				return err
			}
			if insecureHostKey {
				cfg.Spec.SSH.InsecureIgnoreHostKey = true
			}
			if retention > 0 {
				cfg.Spec.Backup.Retention = retention
			}
			if err := storageFlags.applyTo(cfg, insecureHostKey); err != nil {
				return err
			}
			eng := engine.NewEngine(cmd.OutOrStdout())
			return eng.Backup(cmd.Context(), cfg, engine.BackupOptions{
				OutputPath: outputPath,
				Target:     storageFlags.target,
			})
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "Đường dẫn lưu thêm bản sao lưu cục bộ (mặc định: backup-<cluster>-<timestamp>.tar.gz)")
	cmd.Flags().IntVar(&retention, "retention", 0, "Số bản backup mới nhất cần giữ lại (tự xóa bản cũ hơn; 0 = theo cluster.yaml)")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	storageFlags.bind(cmd, true)
	cmd.AddCommand(newBackupScheduleCmd())
	return cmd
}

func newBackupScheduleCmd() *cobra.Command {
	var (
		configFile      string
		onCalendar      string
		retention       int
		insecureHostKey bool
	)
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Cài đặt systemd timer tự động backup etcd & PKI định kỳ trên Primary Control Plane",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			cfg, err := api.LoadClusterConfig(configFile)
			if err != nil {
				return err
			}
			if insecureHostKey {
				cfg.Spec.SSH.InsecureIgnoreHostKey = true
			}
			if retention > 0 {
				cfg.Spec.Backup.Retention = retention
			}
			return engine.NewEngine(cmd.OutOrStdout()).InstallBackupSchedule(cmd.Context(), cfg, onCalendar)
		},
	}
	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVar(&onCalendar, "on-calendar", "", "Lịch chạy systemd OnCalendar (vd: 'daily', '*-*-* 02:00:00'; mặc định lấy từ spec.backup.schedule)")
	cmd.Flags().IntVar(&retention, "retention", 0, "Số bản backup giữ lại trên CP-1 (mặc định: 7)")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}

func newRestoreCmd() *cobra.Command {
	var (
		configFile           string
		backupPath           string
		expectedSHA256       string
		yes                  bool
		forceClusterMismatch bool
		insecureHostKey      bool
		storageFlags         backupStorageFlags
	)

	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Khôi phục etcd & PKI cho cụm 1-node hoặc 3-node HA từ Local, S3 (s3://...) hoặc SFTP (sftp://...)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			if backupPath == "" {
				return fmt.Errorf("cần chỉ định file/URI backup qua cờ --backup (vd: ./backup.tar.gz, s3://bucket/key, sftp:///dir/backup.tar.gz)")
			}
			if !yes {
				return fmt.Errorf("thao tác restore sẽ tạm dừng Control Plane và ghi đè dữ liệu etcd hiện tại; thêm cờ --yes để xác nhận")
			}
			cfg, err := api.LoadClusterConfig(configFile)
			if err != nil {
				return err
			}
			if insecureHostKey {
				cfg.Spec.SSH.InsecureIgnoreHostKey = true
			}
			if err := storageFlags.applyTo(cfg, insecureHostKey); err != nil {
				return err
			}
			eng := engine.NewEngine(cmd.OutOrStdout())
			return eng.Restore(cmd.Context(), cfg, engine.RestoreOptions{
				BackupPath:           backupPath,
				ExpectedSHA256:       expectedSHA256,
				ForceClusterMismatch: forceClusterMismatch,
			})
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVar(&backupPath, "backup", "", "Đường dẫn hoặc URI backup (file cục bộ, s3://bucket/key, hoặc sftp:///path/file.tar.gz) (bắt buộc)")
	cmd.Flags().StringVar(&expectedSHA256, "sha256", "", "Tùy chọn: xác thực mã SHA256 (64-hex) của bản sao lưu trước khi khôi phục")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Xác nhận thực thi khôi phục cụm")
	cmd.Flags().BoolVar(&forceClusterMismatch, "force-cluster-mismatch", false, "Cho phép khôi phục ngay cả khi tên cụm trong backup khác cluster.yaml")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	storageFlags.bind(cmd, false)
	return cmd
}
