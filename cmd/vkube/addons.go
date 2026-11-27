package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/engine"
)

func newAddonsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "addons",
		Short: "Cài đặt bổ sung hoặc đồng bộ lại Core Addons, Labels/Taints & Registry Mirrors trên cụm đang chạy",
	}
	cmd.AddCommand(newAddonsApplyCmd())
	return cmd
}

func newAddonsApplyCmd() *cobra.Command {
	var (
		configFile      string
		bundleFile      string
		manifestPath    string
		insecureHostKey bool
	)
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Áp dụng/cập nhật Core Addons, customManifests, Labels/Taints, Proxy & Registry Mirrors lên cụm đang sống",
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
			return engine.NewEngine(cmd.OutOrStdout()).ApplyAddons(cmd.Context(), cfg, engine.ApplyAddonsOptions{
				BundlePath:   bundleFile,
				ManifestPath: manifestPath,
			})
		},
	}
	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVarP(&bundleFile, "bundle", "b", "", "Đường dẫn tới Offline Bundle (.tar.zst) cho môi trường Air-gapped (0 byte ra Internet)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Đường dẫn file BOM (versions.yaml) tùy chỉnh")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}
