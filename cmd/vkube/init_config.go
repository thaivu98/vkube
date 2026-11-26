package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/templates"
)

func newInitConfigCmd() *cobra.Command {
	var (
		arch   string
		ha     bool
		output string
	)

	cmd := &cobra.Command{
		Use:   "init-config",
		Short: "Sinh file cấu hình mẫu cluster.yaml đầy đủ hướng dẫn (Single-node / Standard / HA + Addons)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := api.ValidateClusterArch(arch); err != nil {
				return err
			}

			content, err := templates.RenderClusterTemplate(templates.ClusterTemplateData{
				Arch: arch,
				HA:   ha,
			})
			if err != nil {
				return err
			}

			if output == "" || output == "-" {
				_, err := cmd.OutOrStdout().Write(content)
				return err
			}
			if err := os.WriteFile(output, content, 0o644); err != nil {
				return fmt.Errorf("write starter config %q: %w", output, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Đã tạo file cấu hình mẫu tại %s (arch=%s, ha=%t)\n", output, arch, ha)
			return nil
		},
	}

	cmd.Flags().StringVar(&arch, "arch", api.ArchAuto, "Kiến trúc CPU của target nodes (mặc định auto — tự phát hiện qua SSH; hỗ trợ amd64, arm64)")
	cmd.Flags().BoolVar(&ha, "ha", false, "Sinh cấu hình cụm HA 3 Control Planes dùng kube-vip")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Đường dẫn file xuất ra (bỏ trống để in ra stdout)")
	return cmd
}
