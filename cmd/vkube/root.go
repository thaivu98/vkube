package main

import (
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vkube",
		Short: "Trình cài đặt Vanilla Kubernetes nhanh dựa trên kubeadm cho On-premise & Air-gapped",
		Long: `vkube là công cụ CLI viết bằng Pure Go giúp đóng gói Offline Bundle (amd64/arm64)
và triển khai cụm Vanilla Kubernetes chuẩn CNCF (containerd + kubeadm + kube-vip + Calico)
nhanh chóng trên môi trường On-premise và Air-gapped.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(
		newVersionCmd(),
		newInitConfigCmd(),
		newBundleCmd(),
		newApplyCmd(),
		newStatusCmd(),
		newResetCmd(),
	)

	return cmd
}
