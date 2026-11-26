package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/versions"
)

func newBundleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Đóng gói, kiểm tra và quản lý Offline Bundle (amd64/arm64)",
	}

	cmd.AddCommand(
		newBundleBuildCmd(),
		newBundleVerifyCmd(),
		newBundleExportManifestCmd(),
	)
	return cmd
}

func newBundleBuildCmd() *cobra.Command {
	var (
		arch         string
		manifestPath string
		outputPath   string
		dryRun       bool
	)

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Tải artifacts & OCI images để đóng gói Offline Bundle (.tar.zst)",
		RunE: func(cmd *cobra.Command, args []string) error {
			bom, err := versions.Load(manifestPath)
			if err != nil {
				return err
			}
			resolved, err := bom.Resolve(arch)
			if err != nil {
				return err
			}

			if outputPath == "" {
				outputPath = filepath.Join("bundles", fmt.Sprintf("vkube-bundle-%s-linux-%s.tar.zst", resolved.ReleaseTag, resolved.Arch))
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Bundle Plan: %s (Kubernetes %s, arch: linux/%s)\n", resolved.ReleaseTag, resolved.KubernetesVersion, resolved.Arch)
			fmt.Fprintf(out, "Output Path: %s\n", outputPath)

			if dryRun {
				fmt.Fprintln(out, "\nArtifacts:")
				for _, a := range resolved.Artifacts {
					fmt.Fprintf(out, "  - [%s] %-16s %s -> %s\n", a.Type, a.Name, a.URL, a.Dest)
				}
				fmt.Fprintln(out, "\nOCI Image Groups:")
				for _, g := range resolved.ImageGroups {
					fmt.Fprintf(out, "  - %s (%s):\n", g.Name, g.Dest)
					for _, img := range g.Images {
						fmt.Fprintf(out, "      * %s\n", img)
					}
				}
				return nil
			}

			return fmt.Errorf("bundle build execution chưa được kích hoạt (sẽ hoàn thiện trong Phase 2 / Task 3–4); dùng --dry-run để xem kế hoạch tải")
		},
	}

	cmd.Flags().StringVar(&arch, "arch", api.DefaultLocalArch(), "Kiến trúc CPU đích của Offline Bundle (mặc định tự nhận theo máy hiện tại; hỗ trợ amd64 hoặc arm64)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Đường dẫn file BOM (versions.yaml) tùy chỉnh để ghi đè phiên bản mặc định")
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "Đường dẫn file .tar.zst đầu ra")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Chỉ in danh sách URL artifact và OCI images sẽ tải mà không thực thi")
	return cmd
}

func newBundleVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify <bundle.tar.zst>",
		Short: "Kiểm tra tính toàn vẹn SHA256 và metadata của file Offline Bundle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bundleFile := args[0]
			if _, err := os.Stat(bundleFile); err != nil {
				return fmt.Errorf("cannot access bundle %q: %w", bundleFile, err)
			}
			return fmt.Errorf("bundle verify sẽ được hoàn thiện trong Phase 2 / Task 5")
		},
	}
	return cmd
}

func newBundleExportManifestCmd() *cobra.Command {
	var outputPath string

	cmd := &cobra.Command{
		Use:   "export-manifest",
		Short: "Xuất file BOM mặc định (versions.yaml) để tùy chỉnh và dùng lại với --manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			data := versions.DefaultBOMBytes()
			if outputPath == "" || outputPath == "-" {
				_, err := cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(outputPath, data, 0o644); err != nil {
				return fmt.Errorf("write manifest %q: %w", outputPath, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Đã xuất file BOM mặc định ra %s\n", outputPath)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "Đường dẫn file xuất ra (bỏ trống để in ra stdout)")
	return cmd
}
