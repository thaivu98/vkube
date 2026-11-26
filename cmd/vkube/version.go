package main

import (
	"fmt"
	"runtime"
	"sort"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/versions"
)

func newVersionCmd() *cobra.Command {
	var manifestPath string

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Hiển thị phiên bản vkube và ma trận thành phần BOM hiện tại",
		RunE: func(cmd *cobra.Command, args []string) error {
			bom, err := versions.Load(manifestPath)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "vkube %s (commit: %s, built: %s, %s/%s)\n",
				version, commit, date, runtime.GOOS, runtime.GOARCH)
			fmt.Fprintf(out, "BOM Release Tag: %s\n", bom.ReleaseTag)
			if manifestPath != "" {
				fmt.Fprintf(out, "BOM Source:      %s\n", manifestPath)
			} else {
				fmt.Fprintf(out, "BOM Source:      embedded (default versions.yaml)\n")
			}

			fmt.Fprintln(out, "Components:")
			keys := make([]string, 0, len(bom.Components))
			for k := range bom.Components {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(out, "  - %-12s %s\n", k+":", bom.Components[k])
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Đường dẫn tới file BOM (versions.yaml) tùy chỉnh để ghi đè mặc định")
	return cmd
}
