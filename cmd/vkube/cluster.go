package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/engine"
)

func newApplyCmd() *cobra.Command {
	var (
		configFile      string
		bundleFile      string
		manifestPath    string
		kubeconfigOut   string
		fastSync        bool
		dryRun          bool
		insecureHostKey bool
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Triển khai cụm Vanilla Kubernetes (Online trực tiếp hoặc Air-gapped với --bundle)",
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

			out := cmd.OutOrStdout()
			if !dryRun {
				fmt.Fprint(out, cliBanner)
				fmt.Fprintf(out, "  vkube %s — Fast Vanilla Kubernetes Installer\n\n", version)
			}
			modeLabel := "Online (trực tiếp)"
			if bundleFile != "" {
				modeLabel = "Air-gapped (--bundle)"
			}
			fmt.Fprintf(out, "Cluster:       %s (arch: linux/%s, CNI: %s, mode: %s)\n", cfg.Metadata.Name, cfg.Spec.Arch, cfg.Spec.Networking.CNI, modeLabel)
			fmt.Fprintf(out, "ControlPlane:  %d node(s)", len(cfg.ControlPlanes()))
			if cfg.Spec.ControlPlaneEndpoint != "" {
				fmt.Fprintf(out, " (endpoint: %s)", cfg.Spec.ControlPlaneEndpoint)
			}
			fmt.Fprintln(out)
			fmt.Fprintf(out, "Workers:       %d node(s)\n", len(cfg.Workers()))
			fmt.Fprintf(out, "Subnets:       pod=%s, service=%s\n", cfg.Spec.Networking.PodSubnet, cfg.Spec.Networking.ServiceSubnet)
			if cfg.Spec.Addons.KongIngress.Enabled && !cfg.Spec.Addons.MetalLB.Enabled {
				fmt.Fprintln(out, "[WARN] kongIngress đang bật nhưng metalLB chưa bật — Service kong-proxy (LoadBalancer) sẽ ở trạng thái <pending> trên Bare-metal/VM nếu không có LB ngoài.")
			}

			if dryRun {
				fmt.Fprintln(out, "\nDry-run hoàn tất: cấu hình cluster.yaml hợp lệ.")
				return nil
			}

			eng := engine.NewEngine(out)
			return eng.Apply(cmd.Context(), cfg, engine.ApplyOptions{
				BundlePath:    bundleFile,
				ManifestPath:  manifestPath,
				KubeconfigOut: kubeconfigOut,
				FastSync:      fastSync,
			})
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVarP(&bundleFile, "bundle", "b", "", "Đường dẫn tới Offline Bundle (.tar.zst) cho môi trường Air-gapped (bỏ trống để cài Online trực tiếp)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Đường dẫn file BOM (versions.yaml) tùy chỉnh khi cài Online")
	cmd.Flags().StringVar(&kubeconfigOut, "kubeconfig-out", "", "Đường dẫn lưu file kubeconfig sau khi cài xong (mặc định: kubeconfig-<cluster>.yaml)")
	cmd.Flags().BoolVar(&fastSync, "fast-sync", false, "Ép bật LAN HTTP BundleServer phát tán Offline Bundle tốc độ cao (mặc định tự bật với >= 3 nodes)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Chỉ kiểm tra cấu hình cluster.yaml và in tóm tắt cụm")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts (chỉ nên dùng cho VM lab hay reset snapshot)")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var (
		configFile      string
		insecureHostKey bool
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Kiểm tra trạng thái sức khỏe các node và thành phần cụm Kubernetes",
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
			return eng.Status(cmd.Context(), cfg)
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}

func newResetCmd() *cobra.Command {
	var (
		configFile      string
		yes             bool
		insecureHostKey bool
	)

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Gỡ bỏ cụm Kubernetes (kubeadm reset) và dọn sạch cấu hình trên các node",
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
			if !yes {
				return fmt.Errorf("thao tác reset sẽ xóa cụm Kubernetes; thêm cờ --yes để xác nhận")
			}
			eng := engine.NewEngine(cmd.OutOrStdout())
			return eng.Reset(cmd.Context(), cfg)
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Xác nhận thực thi gỡ bỏ cụm mà không hỏi lại")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}
