package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/engine"
)

func newCertsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "certs",
		Short: "Kiểm tra thời hạn và gia hạn chứng chỉ Kubernetes trên toàn bộ Control Plane",
	}
	cmd.AddCommand(newCertsCheckCmd(), newCertsRenewCmd())
	return cmd
}

func newCertsCheckCmd() *cobra.Command {
	var (
		configFile      string
		insecureHostKey bool
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Kiểm tra thời hạn hết hạn của toàn bộ chứng chỉ Kubernetes (kubeadm certs check-expiration)",
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
			return engine.NewEngine(cmd.OutOrStdout()).CertsCheck(cmd.Context(), cfg)
		},
	}
	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}

func newCertsRenewCmd() *cobra.Command {
	var (
		configFile      string
		kubeconfigOut   string
		yes             bool
		insecureHostKey bool
	)
	cmd := &cobra.Command{
		Use:   "renew",
		Short: "Gia hạn toàn bộ chứng chỉ Kubernetes tuần tự trên tất cả Control Plane & cập nhật kubeconfig",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			if !yes {
				return fmt.Errorf("thao tác renew sẽ gia hạn chứng chỉ và khởi động lại static pod Control Plane; thêm cờ --yes để xác nhận")
			}
			cfg, err := api.LoadClusterConfig(configFile)
			if err != nil {
				return err
			}
			if insecureHostKey {
				cfg.Spec.SSH.InsecureIgnoreHostKey = true
			}
			return engine.NewEngine(cmd.OutOrStdout()).CertsRenew(cmd.Context(), cfg, kubeconfigOut)
		},
	}
	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVar(&kubeconfigOut, "kubeconfig-out", "", "Đường dẫn lưu file kubeconfig mới sau khi gia hạn")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Xác nhận gia hạn chứng chỉ cụm")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}
