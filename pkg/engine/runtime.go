package engine

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
	"github.com/thaivu98/vkube/pkg/templates"
)

const (
	// Dùng /var/lib/vkube và /var/tmp (luôn nằm trên ổ đĩa thật, tránh /tmp là tmpfs giới hạn RAM trên Ubuntu mới).
	remoteWorkDir     = "/var/lib/vkube/staging"
	remoteManifestDir = "/etc/vkube/manifests"
	remoteImageDir    = "/var/lib/vkube/images"
)

// RunRuntimeAndArtifacts đồng bộ Offline Bundle, giải nén binaries, cấu hình containerd (SystemdCgroup=true),
// tạo systemd unit cho kubelet và nạp toàn bộ OCI images song song trên tất cả các node.
func (e *Engine) RunRuntimeAndArtifacts(ctx context.Context, state *ClusterState) error {
	fmt.Fprintf(e.Out, "==> [3/6] Runtime & Artifacts: Đẩy Offline Bundle, cài containerd/kubelet & nạp OCI images song song...\n")

	// Kiểm tra xem có node nào thiếu binary zstd không; nếu có thì giải nén 1 lần thành .tar tạm bằng Pure Go.
	needPlainTar := false
	for _, n := range state.Config.Spec.Nodes {
		if f := state.Facts[n.Name]; f != nil && !f.HasZstd {
			needPlainTar = true
			break
		}
	}

	var plainTarPath string
	if needPlainTar {
		fmt.Fprintf(e.Out, "  [artifacts] Phát hiện node chưa cài zstd; giải nén lớp zstd cục bộ bằng Pure Go để đảm bảo 100%% Air-gapped...\n")
		var err error
		plainTarPath, err = decompressZstdToTempTar(state.BundlePath)
		if err != nil {
			return fmt.Errorf("decompress bundle zstd locally: %w", err)
		}
		defer os.Remove(plainTarPath)
	}

	pauseVersion := state.BundleManifest.Components["pause"]
	if pauseVersion == "" {
		pauseVersion = "3.10"
	}
	sandboxImage := fmt.Sprintf("registry.k8s.io/pause:%s", pauseVersion)

	return state.Pool.RunParallel(ctx, state.Config.Spec.Nodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
		facts := state.Facts[node.Name]
		useZstd := facts != nil && facts.HasZstd

		localUpload := state.BundlePath
		remoteArchive := "/var/tmp/vkube-bundle.tar.zst"
		extractCmd := fmt.Sprintf("tar --zstd -xf %s -C %s", remoteArchive, remoteWorkDir)
		if !useZstd {
			localUpload = plainTarPath
			remoteArchive = "/var/tmp/vkube-bundle.tar"
			extractCmd = fmt.Sprintf("tar -xf %s -C %s", remoteArchive, remoteWorkDir)
		}

		fmt.Fprintf(e.Out, "  [artifacts] %-12s đang tải Offline Bundle lên node...\n", node.Name)
		if err := r.UploadFile(ctx, localUpload, remoteArchive); err != nil {
			return fmt.Errorf("upload bundle: %w", err)
		}

		// Ghi các file cấu hình systemd & crictl nhúng sẵn vào /tmp trước khi cài bằng sudo
		if err := r.WriteFile(ctx, templates.ContainerdService(), "/tmp/vkube-containerd.service", 0o644); err != nil {
			return err
		}
		if err := r.WriteFile(ctx, templates.KubeletService(), "/tmp/vkube-kubelet.service", 0o644); err != nil {
			return err
		}
		if err := r.WriteFile(ctx, templates.KubeadmDropin(), "/tmp/vkube-10-kubeadm.conf", 0o644); err != nil {
			return err
		}
		if err := r.WriteFile(ctx, templates.CrictlConfig(), "/tmp/vkube-crictl.yaml", 0o644); err != nil {
			return err
		}

		installScript := fmt.Sprintf(`set -euo pipefail
rm -rf %s /tmp/vkube-bundle* && mkdir -p %s /usr/local/bin /opt/cni/bin %s %s /etc/containerd /etc/systemd/system/kubelet.service.d
%s
rm -f %s

install -m 0755 %s/bin/* /usr/local/bin/
if [ -d %s/cni-plugins ]; then
  install -m 0755 %s/cni-plugins/* /opt/cni/bin/
fi
if [ -d %s/manifests ]; then
  cp -f %s/manifests/* %s/
fi
if [ -d %s/images ]; then
  mv -f %s/images/*.tar %s/
fi

mv -f /tmp/vkube-containerd.service /etc/systemd/system/containerd.service
mv -f /tmp/vkube-kubelet.service /etc/systemd/system/kubelet.service
mv -f /tmp/vkube-10-kubeadm.conf /etc/systemd/system/kubelet.service.d/10-kubeadm.conf
mv -f /tmp/vkube-crictl.yaml /etc/crictl.yaml

/usr/local/bin/containerd config default > /etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/g' /etc/containerd/config.toml
sed -i 's|sandbox_image = ".*"|sandbox_image = "%s"|g' /etc/containerd/config.toml

systemctl daemon-reload
systemctl enable --now containerd
systemctl enable kubelet

# Nạp toàn bộ OCI images vào namespace k8s.io của containerd (xóa file tar ngay sau khi nạp để tiết kiệm đĩa)
for img_tar in %s/*.tar; do
  [ -f "$img_tar" ] || continue
  /usr/local/bin/ctr -n k8s.io images import "$img_tar" >/dev/null
  rm -f "$img_tar"
done

rm -rf %s /tmp/vkube-bundle*
`,
			remoteWorkDir, remoteWorkDir, remoteManifestDir, remoteImageDir,
			extractCmd, remoteArchive,
			remoteWorkDir,
			remoteWorkDir, remoteWorkDir,
			remoteWorkDir, remoteWorkDir, remoteManifestDir,
			remoteWorkDir, remoteWorkDir, remoteImageDir,
			sandboxImage,
			remoteImageDir,
			remoteWorkDir,
		)

		fmt.Fprintf(e.Out, "  [runtime]   %-12s cài đặt containerd, kubelet & nạp OCI images...\n", node.Name)
		if _, err := r.RunSudo(ctx, installScript); err != nil {
			return fmt.Errorf("install runtime & import images: %w", err)
		}
		fmt.Fprintf(e.Out, "  [runtime]   %-12s OK\n", node.Name)
		return nil
	})
}

func decompressZstdToTempTar(zstPath string) (string, error) {
	in, err := os.Open(zstPath)
	if err != nil {
		return "", err
	}
	defer in.Close()

	zr, err := zstd.NewReader(in)
	if err != nil {
		return "", err
	}
	defer zr.Close()

	out, err := os.CreateTemp("", "vkube-bundle-*.tar")
	if err != nil {
		return "", err
	}
	outPath := out.Name()
	if _, err := io.Copy(out, zr); err != nil {
		_ = out.Close()
		_ = os.Remove(outPath)
		return "", err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	return outPath, nil
}
