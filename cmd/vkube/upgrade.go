package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/engine"
)

func newUpgradeCmd() *cobra.Command {
	var (
		configFile      string
		targetVersion   string
		bundleFile      string
		manifestPath    string
		skipBackup      bool
		yes             bool
		insecureHostKey bool
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Rolling upgrade cụm Kubernetes không downtime tuần tự từng node (kubeadm upgrade)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			if !yes {
				return fmt.Errorf("thao tác upgrade sẽ nâng cấp lần lượt Control Plane và Worker; hãy backup etcd trước và thêm cờ --yes để xác nhận")
			}
			cfg, err := api.LoadClusterConfig(configFile)
			if err != nil {
				return err
			}
			if insecureHostKey {
				cfg.Spec.SSH.InsecureIgnoreHostKey = true
			}
			return engine.NewEngine(cmd.OutOrStdout()).Upgrade(cmd.Context(), cfg, engine.UpgradeOptions{
				TargetVersion: targetVersion,
				BundlePath:    bundleFile,
				ManifestPath:  manifestPath,
				SkipBackup:    skipBackup,
			})
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVar(&targetVersion, "target-version", "", "Phiên bản Kubernetes đích (vd: v1.36.8; bỏ trống sẽ lấy từ BOM hoặc --bundle)")
	cmd.Flags().StringVarP(&bundleFile, "bundle", "b", "", "Đường dẫn tới Offline Bundle (.tar.zst) phiên bản mới cho môi trường Air-gapped")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Đường dẫn file BOM (versions.yaml) tùy chỉnh khi nâng cấp Online")
	cmd.Flags().BoolVar(&skipBackup, "skip-backup", false, "Bỏ qua bước tự động backup etcd trước khi upgrade")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Xác nhận thực thi rolling upgrade")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}
