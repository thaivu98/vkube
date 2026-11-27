package main

import (
	"github.com/spf13/cobra"
)

const cliBanner = `        _          _
 __   _| | ___   _| |__   ___
 \ \ / / |/ / | | | '_ \ / _ \
  \ V /|   <| |_| | |_) |  __/
   \_/ |_|\_\\__,_|_.__/ \___|
`

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vkube",
		Short: "Trình cài đặt Vanilla Kubernetes nhanh dựa trên kubeadm cho On-premise & Air-gapped",
		Long: cliBanner + `
vkube là công cụ CLI viết bằng Pure Go giúp triển khai cụm Vanilla Kubernetes chuẩn CNCF
(containerd + kubeadm + kube-vip + Calico + Core Addons) nhanh chóng trên môi trường
On-premise (Online trực tiếp) và Air-gapped (Offline Bundle amd64/arm64).`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(
		newVersionCmd(),
		newInitConfigCmd(),
		newBundleCmd(),
		newApplyCmd(),
		newStatusCmd(),
		newAddonsCmd(),
		newAddNodeCmd(),
		newRemoveNodeCmd(),
		newBackupCmd(),
		newRestoreCmd(),
		newCertsCmd(),
		newResetCmd(),
	)

	return cmd
}
