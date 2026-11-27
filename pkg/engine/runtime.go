package engine

import (
	"context"
	"fmt"
	"io"
	"net"
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

		// Ghi các file cấu hình systemd & crictl vào thư mục riêng .vkube-tmp (tránh dùng /tmp dùng chung)
		if err := r.WriteFile(ctx, templates.ContainerdService(), ".vkube-tmp/containerd.service", 0o600); err != nil {
			return err
		}
		if err := r.WriteFile(ctx, templates.KubeletService(), ".vkube-tmp/kubelet.service", 0o600); err != nil {
			return err
		}
		if err := r.WriteFile(ctx, templates.KubeadmDropin(), ".vkube-tmp/10-kubeadm.conf", 0o600); err != nil {
			return err
		}
		if err := r.WriteFile(ctx, templates.CrictlConfig(), ".vkube-tmp/crictl.yaml", 0o600); err != nil {
			return err
		}

		sshUser := state.Config.EffectiveSSH(node).User
		installScript := fmt.Sprintf(`set -euo pipefail
USER_HOME="$(getent passwd %s | cut -d: -f6)"
STAGE_TMP="$USER_HOME/.vkube-tmp"
rm -rf %s && mkdir -p -m 0700 %s && mkdir -p /usr/local/bin /opt/cni/bin %s %s /etc/containerd /etc/containerd/certs.d /etc/systemd/system/kubelet.service.d
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

install -m 0644 "$STAGE_TMP/containerd.service" /etc/systemd/system/containerd.service
install -m 0644 "$STAGE_TMP/kubelet.service" /etc/systemd/system/kubelet.service
install -m 0644 "$STAGE_TMP/10-kubeadm.conf" /etc/systemd/system/kubelet.service.d/10-kubeadm.conf
install -m 0644 "$STAGE_TMP/crictl.yaml" /etc/crictl.yaml
rm -rf "$STAGE_TMP"

# Chỉ gắn --node-ip khi host là địa chỉ IP thực (nếu host là DNS nội bộ thì để kubelet tự phân giải)
%s

/usr/local/bin/containerd config default > /etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/g' /etc/containerd/config.toml
sed -i 's|sandbox_image = ".*"|sandbox_image = "%s"|g' /etc/containerd/config.toml
sed -i 's|config_path = ""|config_path = "/etc/containerd/certs.d"|g' /etc/containerd/config.toml

systemctl daemon-reload
systemctl enable --now containerd
systemctl enable kubelet

# Nạp toàn bộ OCI images vào namespace k8s.io của containerd (xóa file tar ngay sau khi nạp để tiết kiệm đĩa)
for img_tar in %s/*.tar; do
  [ -f "$img_tar" ] || continue
  /usr/local/bin/ctr -n k8s.io images import "$img_tar" >/dev/null
  rm -f "$img_tar"
done

rm -rf %s
`,
			sshUser,
			remoteWorkDir, remoteWorkDir, remoteManifestDir, remoteImageDir,
			extractCmd, remoteArchive,
			remoteWorkDir,
			remoteWorkDir, remoteWorkDir,
			remoteWorkDir, remoteWorkDir, remoteManifestDir,
			remoteWorkDir, remoteWorkDir, remoteImageDir,
			kubeletExtraArgsCmd(node.Host),
			sandboxImage,
			remoteImageDir,
			remoteWorkDir,
		)

		fmt.Fprintf(e.Out, "  [runtime]   %-12s cài đặt containerd, kubelet & nạp OCI images...\n", node.Name)
		if _, err := r.RunSudo(ctx, installScript); err != nil {
			return fmt.Errorf("install runtime & import images: %w", err)
		}
		if len(state.Config.Spec.Registry.Mirrors) > 0 {
			if err := configureRegistryMirrors(ctx, state.Config.Spec.Registry.Mirrors, r, sshUser); err != nil {
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

// kubeletExtraArgsCmd dùng systemd drop-in riêng (20-vkube-node-ip.conf) thay vì ghi đè /etc/default/kubelet,
// và chỉ gắn --node-ip khi host là địa chỉ IP hợp lệ (tránh lỗi kubelet nếu host là tên DNS).
func kubeletExtraArgsCmd(host string) string {
	trimmed := strings.TrimSpace(host)
	if net.ParseIP(trimmed) != nil {
		return fmt.Sprintf(`printf '[Service]\nEnvironment="KUBELET_EXTRA_ARGS=--node-ip=%s"\n' > /etc/systemd/system/kubelet.service.d/20-vkube-node-ip.conf`, trimmed)
	}
	return "rm -f /etc/systemd/system/kubelet.service.d/20-vkube-node-ip.conf"
}

// runRuntimeOnline tải trực tiếp binaries & manifests song song trên từng node,
// kèm kiểm tra SHA256 ngay trên node trước khi cài đặt vào /usr/local/bin.
func (e *Engine) runRuntimeOnline(ctx context.Context, state *ClusterState) error {
	fmt.Fprintf(e.Out, "==> [3/6] Runtime & Artifacts (Online): Tải trực tiếp binaries, verify SHA256 & cấu hình containerd/kubelet song song...\n")

	pauseVersion := state.BundleManifest.Components["pause"]
	if pauseVersion == "" {
		pauseVersion = "3.10"
	}
	sandboxImage := fmt.Sprintf("registry.k8s.io/pause:%s", pauseVersion)

	return state.Pool.RunParallel(ctx, state.Config.Spec.Nodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
		if err := r.WriteFile(ctx, templates.ContainerdService(), ".vkube-tmp/containerd.service", 0o600); err != nil {
			return err
		}
		if err := r.WriteFile(ctx, templates.KubeletService(), ".vkube-tmp/kubelet.service", 0o600); err != nil {
			return err
		}
		if err := r.WriteFile(ctx, templates.KubeadmDropin(), ".vkube-tmp/10-kubeadm.conf", 0o600); err != nil {
			return err
		}
		if err := r.WriteFile(ctx, templates.CrictlConfig(), ".vkube-tmp/crictl.yaml", 0o600); err != nil {
			return err
		}

		sshUser := state.Config.EffectiveSSH(node).User
		var dlCmds []string
		dlCmds = append(dlCmds, "set -euo pipefail")
		dlCmds = append(dlCmds, fmt.Sprintf(`USER_HOME="$(getent passwd %s | cut -d: -f6)"
STAGE_TMP="$USER_HOME/.vkube-tmp"
DL_DIR="/var/lib/vkube/online-dl"
rm -rf "$DL_DIR" && mkdir -p -m 0700 "$DL_DIR"
mkdir -p /usr/local/bin /opt/cni/bin %s /etc/containerd /etc/containerd/certs.d /etc/systemd/system/kubelet.service.d

verify_sha256() {
  local file="$1"
  local sha_url="$2"
  local fname="$3"
  local expected
  expected="$(curl -fsSL "$sha_url" | awk -v f="$fname" '{
    gsub(/^\*/, "", $2);
    if ($2 == f || $2 == "./" f) { print $1; exit }
    if (first == "" && length($1) == 64) first = $1
  } END { if (first != "") print first }' | head -n 1)"
  if [ "${#expected}" -ne 64 ]; then
    echo "Invalid SHA256 from $sha_url" >&2
    return 1
  fi
  echo "$expected  $file" | sha256sum -c - >/dev/null
}`, sshUser, remoteManifestDir))

		for _, art := range state.ResolvedBOM.Artifacts {
			urlFile := path.Base(art.URL)
			dlTarget := fmt.Sprintf(`"$DL_DIR/%s"`, art.Name)
			switch art.Type {
			case versions.ArtifactTypeBinary:
				destPath := path.Join("/usr/local/bin", path.Base(art.Dest))
				dlCmds = append(dlCmds, fmt.Sprintf("curl -fsSL %q -o %s", art.URL, dlTarget))
				if art.SHA256 != "" {
					dlCmds = append(dlCmds, fmt.Sprintf(`echo "%s  %s" | sha256sum -c - >/dev/null`, art.SHA256, strings.Trim(dlTarget, `"`)))
				} else if art.SHA256URL != "" {
					dlCmds = append(dlCmds, fmt.Sprintf("verify_sha256 %s %q %q", dlTarget, art.SHA256URL, urlFile))
				}
				dlCmds = append(dlCmds, fmt.Sprintf("install -m 0755 %s %q && rm -f %s", dlTarget, destPath, dlTarget))
			case versions.ArtifactTypeTarball:
				dlCmds = append(dlCmds, fmt.Sprintf("curl -fsSL %q -o %s", art.URL, dlTarget))
				if art.SHA256 != "" {
					dlCmds = append(dlCmds, fmt.Sprintf(`echo "%s  %s" | sha256sum -c - >/dev/null`, art.SHA256, strings.Trim(dlTarget, `"`)))
				} else if art.SHA256URL != "" {
					dlCmds = append(dlCmds, fmt.Sprintf("verify_sha256 %s %q %q", dlTarget, art.SHA256URL, urlFile))
				}
				switch art.Dest {
				case "cni-plugins":
					dlCmds = append(dlCmds, fmt.Sprintf("tar -xzf %s -C /opt/cni/bin && rm -f %s", dlTarget, dlTarget))
				default:
					if art.Name == "containerd" {
						dlCmds = append(dlCmds, fmt.Sprintf("tar -xzf %s --strip-components=1 -C /usr/local/bin && rm -f %s", dlTarget, dlTarget))
					} else {
						dlCmds = append(dlCmds, fmt.Sprintf("tar -xzf %s -C /usr/local/bin && rm -f %s", dlTarget, dlTarget))
					}
				}
			case versions.ArtifactTypeManifest:
				if node.Role == api.RoleControlPlane {
					destPath := path.Join(remoteManifestDir, path.Base(art.Dest))
					dlCmds = append(dlCmds, fmt.Sprintf("curl -fsSL %q -o %s", art.URL, dlTarget))
					if art.SHA256 != "" {
						dlCmds = append(dlCmds, fmt.Sprintf(`echo "%s  %s" | sha256sum -c - >/dev/null`, art.SHA256, strings.Trim(dlTarget, `"`)))
					} else if art.SHA256URL != "" {
						dlCmds = append(dlCmds, fmt.Sprintf("verify_sha256 %s %q %q", dlTarget, art.SHA256URL, urlFile))
					}
					dlCmds = append(dlCmds, fmt.Sprintf("install -m 0644 %s %q && rm -f %s", dlTarget, destPath, dlTarget))
				}
			}
		}

		dlCmds = append(dlCmds, fmt.Sprintf(`rm -rf "$DL_DIR"
install -m 0644 "$STAGE_TMP/containerd.service" /etc/systemd/system/containerd.service
install -m 0644 "$STAGE_TMP/kubelet.service" /etc/systemd/system/kubelet.service
install -m 0644 "$STAGE_TMP/10-kubeadm.conf" /etc/systemd/system/kubelet.service.d/10-kubeadm.conf
install -m 0644 "$STAGE_TMP/crictl.yaml" /etc/crictl.yaml
rm -rf "$STAGE_TMP"

# Chỉ gắn --node-ip khi host là địa chỉ IP thực (nếu host là DNS nội bộ thì để kubelet tự phân giải)
%s

/usr/local/bin/containerd config default > /etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/g' /etc/containerd/config.toml
sed -i 's|sandbox_image = ".*"|sandbox_image = "%s"|g' /etc/containerd/config.toml
sed -i 's|config_path = ""|config_path = "/etc/containerd/certs.d"|g' /etc/containerd/config.toml

systemctl daemon-reload
systemctl enable --now containerd
systemctl enable kubelet`, kubeletExtraArgsCmd(node.Host), sandboxImage))

		fmt.Fprintf(e.Out, "  [online]    %-12s đang tải binaries, verify SHA256 & cài containerd/kubelet...\n", node.Name)
		if _, err := r.RunSudo(ctx, strings.Join(dlCmds, "\n")); err != nil {
			return fmt.Errorf("online runtime setup failed: %w", err)
		}
		if len(state.Config.Spec.Registry.Mirrors) > 0 {
			if err := configureRegistryMirrors(ctx, state.Config.Spec.Registry.Mirrors, r, sshUser); err != nil {
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
