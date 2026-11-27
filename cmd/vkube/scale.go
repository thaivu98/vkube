package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/engine"
)

func newAddNodeCmd() *cobra.Command {
	var (
		configFile      string
		nodeName        string
		bundleFile      string
		manifestPath    string
		fastSync        bool
		insecureHostKey bool
	)

	cmd := &cobra.Command{
		Use:   "add-node",
		Short: "Thêm Worker hoặc Control Plane mới vào cụm Kubernetes đang chạy",
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
			return eng.AddNodes(cmd.Context(), cfg, engine.AddNodeOptions{
				NodeName:     nodeName,
				BundlePath:   bundleFile,
				ManifestPath: manifestPath,
				FastSync:     fastSync,
			})
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml đã bổ sung node mới (bắt buộc)")
	cmd.Flags().StringVar(&nodeName, "node", "", "Tên cụ thể của node cần thêm (bỏ trống để tự phát hiện toàn bộ node mới trong cluster.yaml)")
	cmd.Flags().StringVarP(&bundleFile, "bundle", "b", "", "Đường dẫn tới Offline Bundle (.tar.zst) cho môi trường Air-gapped")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Đường dẫn file BOM (versions.yaml) tùy chỉnh khi thêm node Online")
	cmd.Flags().BoolVar(&fastSync, "fast-sync", false, "Bật LAN HTTP BundleServer để phát tán Offline Bundle tốc độ cao")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}

func newRemoveNodeCmd() *cobra.Command {
	var (
		configFile      string
		nodeName        string
		yes             bool
		skipReset       bool
		insecureHostKey bool
	)

	cmd := &cobra.Command{
		Use:   "remove-node",
		Short: "Drain, gỡ etcd member (nếu là CP), xóa node khỏi cụm và reset máy an toàn",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("cần chỉ định file cấu hình cụm qua cờ -f / --file")
			}
			if nodeName == "" {
				return fmt.Errorf("cần chỉ định tên node cần gỡ qua cờ --node")
			}
			if !yes {
				return fmt.Errorf("thao tác remove-node sẽ drain và gỡ node %q khỏi cụm; thêm cờ --yes để xác nhận", nodeName)
			}
			cfg, err := api.LoadClusterConfig(configFile)
			if err != nil {
				return err
			}
			if insecureHostKey {
				cfg.Spec.SSH.InsecureIgnoreHostKey = true
			}
			eng := engine.NewEngine(cmd.OutOrStdout())
			return eng.RemoveNode(cmd.Context(), cfg, engine.RemoveNodeOptions{
				NodeName:  nodeName,
				SkipReset: skipReset,
			})
		},
	}

	cmd.Flags().StringVarP(&configFile, "file", "f", "", "Đường dẫn tới file cluster.yaml (bắt buộc)")
	cmd.Flags().StringVar(&nodeName, "node", "", "Tên node cần gỡ khỏi cụm (bắt buộc)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Xác nhận gỡ node khỏi cụm")
	cmd.Flags().BoolVar(&skipReset, "skip-reset", false, "Chỉ drain & xóa khỏi Kubernetes API/etcd, không SSH reset máy đích")
	cmd.Flags().BoolVar(&insecureHostKey, "insecure-skip-host-key-check", false, "Bỏ qua xác thực SSH known_hosts")
	return cmd
}
