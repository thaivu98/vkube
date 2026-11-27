package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/bundle"
	"github.com/thaivu98/vkube/pkg/ssh"
	"github.com/thaivu98/vkube/pkg/versions"
)

// UpgradeOptions chứa tham số cho lệnh `vkube upgrade`.
type UpgradeOptions struct {
	TargetVersion string // Phiên bản K8s đích (bỏ trống sẽ lấy từ BOM hoặc Offline Bundle)
	BundlePath    string // Nếu có -> nâng cấp Air-gapped từ Offline Bundle mới
	ManifestPath  string // File BOM tùy chỉnh (chế độ Online)
}

// Upgrade thực hiện rolling upgrade cụm Kubernetes an toàn từng node một (zero-downtime):
// CP-1 (kubeadm upgrade apply) -> Secondary CPs tuần tự (kubeadm upgrade node) -> Workers tuần tự (drain -> upgrade -> uncordon).
func (e *Engine) Upgrade(ctx context.Context, cfg *api.ClusterConfig, opts UpgradeOptions) error {
	start := time.Now()
	onlineMode := strings.TrimSpace(opts.BundlePath) == ""

	var (
		manifest  *api.BundleManifest
		bom       *versions.BOM
		targetVer string
		err       error
	)
	if onlineMode {
		bom, err = versions.Load(opts.ManifestPath)
		if err != nil {
			return fmt.Errorf("load BOM manifest: %w", err)
		}
		targetVer = strings.TrimSpace(opts.TargetVersion)
		if targetVer == "" {
			targetVer = bom.Components["kubernetes"]
		}
	} else {
		manifest, err = bundle.Verify(opts.BundlePath)
		if err != nil {
			return fmt.Errorf("verify upgrade bundle: %w", err)
		}
		targetVer = strings.TrimSpace(opts.TargetVersion)
		if targetVer == "" {
			targetVer = manifest.KubernetesVersion
		}
	}
	if !strings.HasPrefix(targetVer, "v") {
		targetVer = "v" + targetVer
	}

	pool, err := ssh.ConnectAll(ctx, cfg, e.Out)
	if err != nil {
		return err
	}
	defer pool.Close()

	cps := cfg.ControlPlanes()
	workers := cfg.Workers()
	cp1 := cps[0]
	cp1Runner, err := pool.Get(cp1.Name)
	if err != nil {
		return err
	}

	// Kiểm tra phiên bản hiện tại của cụm và chặn downgrade hoặc nhảy quá 1 minor version
	curRes, err := cp1Runner.RunSudo(ctx, `/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get node `+cp1.Name+` -o jsonpath='{.status.nodeInfo.kubeletVersion}'`)
	if err != nil {
		return fmt.Errorf("query current cluster version on %s: %w", cp1.Name, err)
	}
	currentVer := strings.TrimSpace(curRes.Stdout)
	if err := validateUpgradePath(currentVer, targetVer); err != nil {
		return err
	}

	// Phát hiện kiến trúc CPU của cụm
	archRes, err := cp1Runner.RunSudo(ctx, "uname -m")
	if err != nil {
		return fmt.Errorf("detect arch on %s: %w", cp1.Name, err)
	}
	arch, err := normalizeLinuxArch(archRes.Stdout)
	if err != nil {
		return err
	}
	if !onlineMode && manifest.Arch != arch {
		return fmt.Errorf("upgrade bundle arch %q does not match cluster arch %q", manifest.Arch, arch)
	}

	fmt.Fprintf(e.Out, "==> Bắt đầu Rolling Upgrade cụm %q: %s -> %s (%d CP, %d Worker)...\n",
		cfg.Metadata.Name, currentVer, targetVer, len(cps), len(workers))

	// Helper chuẩn bị binary kubeadm/kubelet/kubectl mới (và nạp images nếu Air-gapped) trên 1 node
	stageNodeBinaries := func(node api.NodeConfig, r ssh.Runner) error {
		if onlineMode {
			proxyExp := proxyShellExports(cfg)
			cmd := fmt.Sprintf(`set -euo pipefail
%s
DL_DIR="/var/lib/vkube/upgrade-dl"
rm -rf "$DL_DIR" && mkdir -p -m 0700 "$DL_DIR"
trap 'rm -rf "$DL_DIR"' EXIT
for bin in kubeadm kubelet kubectl; do
  URL="https://dl.k8s.io/release/%s/bin/linux/%s/$bin"
  curl -fsSL "$URL" -o "$DL_DIR/$bin"
  SHA="$(curl -fsSL "$URL.sha256" | awk '{print $1}')"
  echo "$SHA  $DL_DIR/$bin" | sha256sum -c - >/dev/null
done
install -m 0755 "$DL_DIR/kubeadm" /usr/local/bin/kubeadm
cp -f "$DL_DIR/kubelet" /var/lib/vkube/kubelet.new
cp -f "$DL_DIR/kubectl" /var/lib/vkube/kubectl.new
chmod 0755 /var/lib/vkube/kubelet.new /var/lib/vkube/kubectl.new
`, proxyExp, targetVer, arch)
			_, err := r.RunSudo(ctx, cmd)
			return err
		}

		// Chế độ Air-gapped: đẩy bundle mới, giải nén binary mới và nạp OCI images mới
		remoteArchive := "/var/tmp/vkube-upgrade-bundle.tar.zst"
		if err := r.UploadFile(ctx, opts.BundlePath, remoteArchive); err != nil {
			return fmt.Errorf("upload upgrade bundle: %w", err)
		}
		cmd := fmt.Sprintf(`set -euo pipefail
UDIR="/var/lib/vkube/upgrade-staging"
rm -rf "$UDIR" && mkdir -p -m 0700 "$UDIR"
trap 'rm -rf "$UDIR" %s' EXIT
tar --zstd -xf %s -C "$UDIR"
install -m 0755 "$UDIR/bin/kubeadm" /usr/local/bin/kubeadm
cp -f "$UDIR/bin/kubelet" /var/lib/vkube/kubelet.new
cp -f "$UDIR/bin/kubectl" /var/lib/vkube/kubectl.new
chmod 0755 /var/lib/vkube/kubelet.new /var/lib/vkube/kubectl.new
if [ -d "$UDIR/images" ]; then
  for img_tar in "$UDIR/images"/*.tar; do
    [ -f "$img_tar" ] || continue
    /usr/local/bin/ctr -n k8s.io images import "$img_tar" >/dev/null
    rm -f "$img_tar"
  done
fi
`, remoteArchive, remoteArchive)
		_, err := r.RunSudo(ctx, cmd)
		return err
	}

	swapKubeletAndUncordon := func(node api.NodeConfig, r ssh.Runner) error {
		drainCmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf drain %s --ignore-daemonsets --delete-emptydir-data --force --timeout=180s", node.Name)
		if _, err := cp1Runner.RunSudo(ctx, drainCmd); err != nil {
			return fmt.Errorf("drain %s: %w", node.Name, err)
		}
		swapCmd := `set -euo pipefail
systemctl stop kubelet
mv -f /var/lib/vkube/kubelet.new /usr/local/bin/kubelet
mv -f /var/lib/vkube/kubectl.new /usr/local/bin/kubectl
systemctl daemon-reload
systemctl start kubelet`
		if _, err := r.RunSudo(ctx, swapCmd); err != nil {
			return fmt.Errorf("upgrade kubelet/kubectl on %s: %w", node.Name, err)
		}
		uncordonCmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf uncordon %s", node.Name)
		if _, err := cp1Runner.RunSudo(ctx, uncordonCmd); err != nil {
			return fmt.Errorf("uncordon %s: %w", node.Name, err)
		}
		waitCmd := fmt.Sprintf("/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf wait --for=condition=Ready node/%s --timeout=180s", node.Name)
		if _, err := cp1Runner.RunSudo(ctx, waitCmd); err != nil {
			return fmt.Errorf("wait Ready after upgrade on %s: %w", node.Name, err)
		}
		return nil
	}

	// 1. Nâng cấp Primary Control Plane (CP-1)
	fmt.Fprintf(e.Out, "  [upgrade]   %-12s chuẩn bị binaries & chạy kubeadm upgrade apply %s...\n", cp1.Name, targetVer)
	if err := stageNodeBinaries(cp1, cp1Runner); err != nil {
		return fmt.Errorf("stage binaries on %s: %w", cp1.Name, err)
	}
	if _, err := cp1Runner.RunSudo(ctx, fmt.Sprintf("/usr/local/bin/kubeadm upgrade apply %s -y", targetVer)); err != nil {
		return fmt.Errorf("kubeadm upgrade apply %s on %s: %w", targetVer, cp1.Name, err)
	}
	if err := swapKubeletAndUncordon(cp1, cp1Runner); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  [upgrade]   %-12s OK (%s)\n", cp1.Name, targetVer)

	// 2. Nâng cấp tuần tự Secondary Control Planes
	for _, cp := range cps[1:] {
		r, err := pool.Get(cp.Name)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "  [upgrade]   %-12s chuẩn bị binaries & chạy kubeadm upgrade node...\n", cp.Name)
		if err := stageNodeBinaries(cp, r); err != nil {
			return fmt.Errorf("stage binaries on %s: %w", cp.Name, err)
		}
		if _, err := r.RunSudo(ctx, "/usr/local/bin/kubeadm upgrade node"); err != nil {
			return fmt.Errorf("kubeadm upgrade node on %s: %w", cp.Name, err)
		}
		if err := swapKubeletAndUncordon(cp, r); err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "  [upgrade]   %-12s OK (%s)\n", cp.Name, targetVer)
	}

	// 3. Nâng cấp tuần tự Worker Nodes để bảo vệ PodDisruptionBudget
	for _, wk := range workers {
		r, err := pool.Get(wk.Name)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "  [upgrade]   %-12s chuẩn bị binaries & chạy kubeadm upgrade node...\n", wk.Name)
		if err := stageNodeBinaries(wk, r); err != nil {
			return fmt.Errorf("stage binaries on %s: %w", wk.Name, err)
		}
		if _, err := r.RunSudo(ctx, "/usr/local/bin/kubeadm upgrade node"); err != nil {
			return fmt.Errorf("kubeadm upgrade node on %s: %w", wk.Name, err)
		}
		if err := swapKubeletAndUncordon(wk, r); err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "  [upgrade]   %-12s OK (%s)\n", wk.Name, targetVer)
	}

	_ = e.printClusterStatus(ctx, cfg.Metadata.Name, cp1, cp1Runner)
	fmt.Fprintf(e.Out, "\nRolling Upgrade cụm %q lên %s hoàn tất sau %s!\n", cfg.Metadata.Name, targetVer, time.Since(start).Round(time.Second))
	return nil
}

// validateUpgradePath kiểm tra quy tắc nâng cấp chuẩn Kubernetes:
// không cho phép downgrade và không nhảy vượt quá 1 minor version mỗi lần (ví dụ 1.35 -> 1.36 OK, 1.34 -> 1.36 bị chặn).
func validateUpgradePath(currentVer, targetVer string) error {
	cMaj, cMin, cPat, err := parseSemver(currentVer)
	if err != nil {
		return fmt.Errorf("parse current version %q: %w", currentVer, err)
	}
	tMaj, tMin, tPat, err := parseSemver(targetVer)
	if err != nil {
		return fmt.Errorf("parse target version %q: %w", targetVer, err)
	}
	if tMaj != cMaj {
		return fmt.Errorf("major version upgrade from %s to %s is not supported", currentVer, targetVer)
	}
	if tMin < cMin || (tMin == cMin && tPat < cPat) {
		return fmt.Errorf("downgrade from %s to %s is not allowed", currentVer, targetVer)
	}
	if tMin-cMin > 1 {
		return fmt.Errorf("cannot skip minor versions (%s -> %s); Kubernetes requires upgrading one minor version at a time", currentVer, targetVer)
	}
	if tMin == cMin && tPat == cPat {
		return fmt.Errorf("cluster is already running Kubernetes %s", currentVer)
	}
	return nil
}

func parseSemver(v string) (major, minor, patch int, err error) {
	s := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if idx := strings.IndexAny(s, "-+"); idx >= 0 {
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 3 {
		return 0, 0, 0, fmt.Errorf("invalid semver %q", v)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, err
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, err
	}
	patch, err = strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0, err
	}
	return major, minor, patch, nil
}
