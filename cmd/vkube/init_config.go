package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/thaivu98/vkube/pkg/api"
)

func newInitConfigCmd() *cobra.Command {
	var (
		arch   string
		ha     bool
		output string
	)

	cmd := &cobra.Command{
		Use:   "init-config",
		Short: "Sinh file cấu hình mẫu cluster.yaml cho cụm Single-node hoặc HA",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := api.ValidateClusterArch(arch); err != nil {
				return err
			}

			content := renderStarterClusterYAML(arch, ha)
			if output == "" || output == "-" {
				fmt.Fprint(cmd.OutOrStdout(), content)
				return nil
			}
			if err := os.WriteFile(output, []byte(content), 0o644); err != nil {
				return fmt.Errorf("write starter config %q: %w", output, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Đã tạo file cấu hình mẫu tại %s (arch=%s, ha=%t)\n", output, arch, ha)
			return nil
		},
	}

	cmd.Flags().StringVar(&arch, "arch", api.DefaultLocalArch(), "Kiến trúc CPU của target nodes (mặc định tự nhận theo máy hiện tại; hỗ trợ amd64, arm64 hoặc auto)")
	cmd.Flags().BoolVar(&ha, "ha", false, "Sinh cấu hình cụm HA 3 Control Planes dùng kube-vip")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Đường dẫn file xuất ra (bỏ trống để in ra stdout)")
	return cmd
}

func renderStarterClusterYAML(arch string, ha bool) string {
	if ha {
		return fmt.Sprintf(`apiVersion: vkube.io/v1alpha1
kind: Cluster
metadata:
  name: vkube-ha
spec:
  arch: %s
  vip: 192.0.2.100
  # vipInterface: ens160  # Bỏ trống để tự động phát hiện card mạng mặc định
  networking:
    podSubnet: 10.244.0.0/16
    serviceSubnet: 10.96.0.0/12
    cni: calico
  ssh:
    user: ubuntu
    port: 22
    privateKeyPath: ~/.ssh/id_ed25519
  nodes:
    - name: cp-1
      host: 192.0.2.11
      role: control-plane
    - name: cp-2
      host: 192.0.2.12
      role: control-plane
    - name: cp-3
      host: 192.0.2.13
      role: control-plane
    - name: worker-1
      host: 192.0.2.21
      role: worker
    - name: worker-2
      host: 192.0.2.22
      role: worker
`, arch)
	}

	return fmt.Sprintf(`apiVersion: vkube.io/v1alpha1
kind: Cluster
metadata:
  name: vkube-single
spec:
  arch: %s
  networking:
    podSubnet: 10.244.0.0/16
    serviceSubnet: 10.96.0.0/12
    cni: calico
  ssh:
    user: ubuntu
    port: 22
    privateKeyPath: ~/.ssh/id_ed25519
  nodes:
    - name: cp-1
      host: 192.0.2.10
      role: control-plane
    - name: worker-1
      host: 192.0.2.11
      role: worker
`, arch)
}
