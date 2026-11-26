package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
	"github.com/thaivu98/vkube/pkg/templates"
	"github.com/thaivu98/vkube/pkg/versions"
)

const (
	// Dùng /var/lib/vkube và /var/tmp (luôn nằm trên ổ đĩa thật, tránh /tmp là tmpfs giới hạn RAM trên Ubuntu mới).
	remoteWorkDir     = "/var/lib/vkube/staging"
	remoteManifestDir = "/etc/vkube/manifests"
	remoteImageDir    = "/var/lib/vkube/images"
)

// RunRuntimeAndArtifacts cài đặt binaries, containerd (SystemdCgroup=true), kubelet và images
// song song trên tất cả các node (tự động chọn chế độ Online trực tiếp hoặc Offline Bundle).
func (e *Engine) RunRuntimeAndArtifacts(ctx context.Context, state *ClusterState) error {
	if state.OnlineMode {
		return e.runRuntimeOnline(ctx, state)
	}
	fmt.Fprintf(e.Out, "==> [3/6] Runtime & Artifacts (Air-gapped): Đẩy Offline Bundle, cài containerd/kubelet & nạp OCI images song song...\n")

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
sed -i 's|config_path = ""|config_path = "/etc/containerd/certs.d"|g' /etc/containerd/config.toml
mkdir -p /etc/containerd/certs.d

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
		if len(state.Config.Spec.Registry.Mirrors) > 0 {
			if err := configureRegistryMirrors(ctx, state.Config.Spec.Registry.Mirrors, r); err != nil {
				return fmt.Errorf("configure private registry mirrors: %w", err)
			}
			if _, err := r.RunSudo(ctx, "systemctl restart containerd"); err != nil {
				return fmt.Errorf("restart containerd after registry mirror config: %w", err)
			}
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

// runRuntimeOnline tải trực tiếp binaries & manifests song song trên từng node (không cần build Offline Bundle).
func (e *Engine) runRuntimeOnline(ctx context.Context, state *ClusterState) error {
	fmt.Fprintf(e.Out, "==> [3/6] Runtime & Artifacts (Online): Tải trực tiếp binaries & cấu hình containerd/kubelet song song...\n")

	pauseVersion := state.BundleManifest.Components["pause"]
	if pauseVersion == "" {
		pauseVersion = "3.10"
	}
	sandboxImage := fmt.Sprintf("registry.k8s.io/pause:%s", pauseVersion)

	return state.Pool.RunParallel(ctx, state.Config.Spec.Nodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
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

		var dlCmds []string
		dlCmds = append(dlCmds, "set -euo pipefail")
		dlCmds = append(dlCmds, fmt.Sprintf("mkdir -p /usr/local/bin /opt/cni/bin %s /etc/containerd /etc/containerd/certs.d /etc/systemd/system/kubelet.service.d", remoteManifestDir))

		for _, art := range state.ResolvedBOM.Artifacts {
			switch art.Type {
			case versions.ArtifactTypeBinary:
				destPath := path.Join("/usr/local/bin", path.Base(art.Dest))
				dlCmds = append(dlCmds, fmt.Sprintf("curl -fsSL %q -o %q && chmod 0755 %q", art.URL, destPath, destPath))
			case versions.ArtifactTypeTarball:
				switch art.Dest {
				case "cni-plugins":
					dlCmds = append(dlCmds, fmt.Sprintf("curl -fsSL %q | tar -xzf - -C /opt/cni/bin", art.URL))
				default:
					if art.Name == "containerd" {
						dlCmds = append(dlCmds, fmt.Sprintf("curl -fsSL %q | tar -xzf - --strip-components=1 -C /usr/local/bin", art.URL))
					} else {
						dlCmds = append(dlCmds, fmt.Sprintf("curl -fsSL %q | tar -xzf - -C /usr/local/bin", art.URL))
					}
				}
			case versions.ArtifactTypeManifest:
				if node.Role == api.RoleControlPlane {
					destPath := path.Join(remoteManifestDir, path.Base(art.Dest))
					dlCmds = append(dlCmds, fmt.Sprintf("curl -fsSL %q -o %q", art.URL, destPath))
				}
			}
		}

		dlCmds = append(dlCmds, fmt.Sprintf(`mv -f /tmp/vkube-containerd.service /etc/systemd/system/containerd.service
mv -f /tmp/vkube-kubelet.service /etc/systemd/system/kubelet.service
mv -f /tmp/vkube-10-kubeadm.conf /etc/systemd/system/kubelet.service.d/10-kubeadm.conf
mv -f /tmp/vkube-crictl.yaml /etc/crictl.yaml

/usr/local/bin/containerd config default > /etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/g' /etc/containerd/config.toml
sed -i 's|sandbox_image = ".*"|sandbox_image = "%s"|g' /etc/containerd/config.toml
sed -i 's|config_path = ""|config_path = "/etc/containerd/certs.d"|g' /etc/containerd/config.toml

systemctl daemon-reload
systemctl enable --now containerd
systemctl enable kubelet`, sandboxImage))

		fmt.Fprintf(e.Out, "  [online]    %-12s đang tải binaries & cài đặt containerd/kubelet...\n", node.Name)
		if _, err := r.RunSudo(ctx, strings.Join(dlCmds, "\n")); err != nil {
			return fmt.Errorf("online runtime setup failed: %w", err)
		}
		if len(state.Config.Spec.Registry.Mirrors) > 0 {
			if err := configureRegistryMirrors(ctx, state.Config.Spec.Registry.Mirrors, r); err != nil {
				return fmt.Errorf("configure private registry mirrors: %w", err)
			}
			if _, err := r.RunSudo(ctx, "systemctl restart containerd"); err != nil {
				return fmt.Errorf("restart containerd after registry mirror config: %w", err)
			}
		}
		fmt.Fprintf(e.Out, "  [online]    %-12s OK\n", node.Name)
		return nil
	})
}
