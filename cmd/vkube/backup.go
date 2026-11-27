package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/engine"
)

func newBackupCmd() *cobra.Command {
	var (
		configFile      string
		outputPath      string
		insecureHostKey bool
	)

	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Chụp snapshot etcd (có verify) & sao lưu /etc/kubernetes/pki về máy Deployer",
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
			eng := engine.NewEngine(cmd.OutOrStdout())
			return eng.Backup(cmd.Context(), cfg, engine.BackupOptions{
				OutputPath: outputPath,
			})
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "Đường dẫn file .tar.gz lưu bản sao lưu (mặc định: backup-<cluster>-<timestamp>.tar.gz)")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}

func newRestoreCmd() *cobra.Command {
	var (
		configFile      string
		backupPath      string
		yes             bool
		insecureHostKey bool
	)

	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Khôi phục etcd & PKI cho cụm 1-node hoặc 3-node HA từ file backup (chống split-brain)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			if backupPath == "" {
				return fmt.Errorf("cần chỉ định file backup .tar.gz qua cờ --backup")
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
			eng := engine.NewEngine(cmd.OutOrStdout())
			return eng.Restore(cmd.Context(), cfg, engine.RestoreOptions{
				BackupPath: backupPath,
			})
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVar(&backupPath, "backup", "", "Đường dẫn file backup .tar.gz đã tạo bởi `vkube backup` (bắt buộc)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Xác nhận thực thi khôi phục cụm")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}
