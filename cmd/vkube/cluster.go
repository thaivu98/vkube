package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
)

func newApplyCmd() *cobra.Command {
	var (
		configFile string
		bundleFile string
		dryRun     bool
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Triển khai cụm Vanilla Kubernetes từ cluster.yaml và Offline Bundle",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			cfg, err := api.LoadClusterConfig(configFile)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Cluster:       %s (arch: linux/%s, CNI: %s)\n", cfg.Metadata.Name, cfg.Spec.Arch, cfg.Spec.Networking.CNI)
			fmt.Fprintf(out, "ControlPlane:  %d node(s)", len(cfg.ControlPlanes()))
			if cfg.Spec.ControlPlaneEndpoint != "" {
				fmt.Fprintf(out, " (endpoint: %s)", cfg.Spec.ControlPlaneEndpoint)
			}
			fmt.Fprintln(out)
			fmt.Fprintf(out, "Workers:       %d node(s)\n", len(cfg.Workers()))
			fmt.Fprintf(out, "Subnets:       pod=%s, service=%s\n", cfg.Spec.Networking.PodSubnet, cfg.Spec.Networking.ServiceSubnet)

			if dryRun {
				fmt.Fprintln(out, "\nDry-run hoàn tất: cấu hình cluster.yaml hợp lệ.")
				return nil
			}
			if bundleFile == "" {
				return fmt.Errorf("cần chỉ định file Offline Bundle qua cờ --bundle")
			}
			return fmt.Errorf("cluster provisioning engine sẽ được hoàn thiện trong Phase 3–4 (Task 6–10)")
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVarP(&bundleFile, "bundle", "b", "", "Đường dẫn tới file Offline Bundle (.tar.zst)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Chỉ kiểm tra cấu hình cluster.yaml và in tóm tắt cụm")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var configFile string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Kiểm tra trạng thái sức khỏe các node và thành phần cụm Kubernetes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			if _, err := api.LoadClusterConfig(configFile); err != nil {
				return err
			}
			return fmt.Errorf("lệnh status sẽ được hoàn thiện trong Phase 4 / Task 10")
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	return cmd
}

func newResetCmd() *cobra.Command {
	var (
		configFile string
		yes        bool
	)

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Gỡ bỏ cụm Kubernetes (kubeadm reset) và dọn sạch cấu hình trên các node",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			if _, err := api.LoadClusterConfig(configFile); err != nil {
				return err
			}
			if !yes {
				return fmt.Errorf("thao tác reset sẽ xóa cụm Kubernetes; thêm cờ --yes để xác nhận")
			}
			return fmt.Errorf("lệnh reset sẽ được hoàn thiện trong Phase 4 / Task 10")
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Xác nhận thực thi gỡ bỏ cụm mà không hỏi lại")
	return cmd
}
