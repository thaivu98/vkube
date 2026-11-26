package engine

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

const preflightProbeScript = `set -euo pipefail
OS_ID=""
OS_VER=""
if [ -f /etc/os-release ]; then
  . /etc/os-release
  OS_ID="${ID:-}"
  OS_VER="${VERSION_ID:-}"
fi
ARCH="$(uname -m)"
KERNEL="$(uname -r)"
HNAME="$(hostname)"
IFACE="$(ip route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')"
CPUS="$(nproc 2>/dev/null || echo 1)"
MEM_KB="$(awk '/MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
HAS_ZSTD="0"
if command -v zstd >/dev/null 2>&1; then HAS_ZSTD="1"; fi
GOLDEN="0"
if [ -f /etc/vkube-golden-image ]; then GOLDEN="1"; fi
printf "OS_ID=%s\nOS_VER=%s\nARCH=%s\nKERNEL=%s\nHOSTNAME=%s\nIFACE=%s\nCPUS=%s\nMEM_MB=%s\nHAS_ZSTD=%s\nGOLDEN=%s\n" \
  "$OS_ID" "$OS_VER" "$ARCH" "$KERNEL" "$HNAME" "$IFACE" "$CPUS" "$((MEM_KB / 1024))" "$HAS_ZSTD" "$GOLDEN"
`

// RunPreflight thu thập NodeFacts đúng 1 lần song song trên toàn bộ nodes, lưu vào state.Facts
// và kiểm tra OS, kiến trúc khớp Offline Bundle, tài nguyên tối thiểu cũng như chống trùng IP VIP.
func (e *Engine) RunPreflight(ctx context.Context, state *ClusterState) error {
	fmt.Fprintf(e.Out, "==> [1/6] Preflight: Thu thập NodeFacts (1 lần duy nhất) & kiểm tra %d node(s)...\n", len(state.Config.Spec.Nodes))

	var mu sync.Mutex
	err := state.Pool.RunParallel(ctx, state.Config.Spec.Nodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
		res, err := r.RunSudo(ctx, preflightProbeScript)
		if err != nil {
			return fmt.Errorf("probe node facts: %w", err)
		}
		facts, err := parseNodeFacts(node.Name, node.Host, res.Stdout)
		if err != nil {
			return fmt.Errorf("parse node facts: %w", err)
		}

		if err := validateNodeFacts(node, facts, state.Config, state.BundleManifest); err != nil {
			return err
		}

		mu.Lock()
		state.Facts[node.Name] = facts
		fmt.Fprintf(e.Out, "  [preflight] %-12s %s %s | arch=%s | nic=%s | %d vCPU / %d MB RAM | zstd=%t\n",
			node.Name, facts.OSID, facts.OSVersion, facts.Arch, facts.DefaultInterface, facts.CPUCores, facts.MemoryMB, facts.HasZstd)
		mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}

	// Kiểm tra chống trùng IP VIP trên mạng LAN thực tế từ Primary Control Plane.
	if state.Config.Spec.VIP != "" {
		cp1 := state.Config.ControlPlanes()[0]
		r, err := state.Pool.Get(cp1.Name)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "  [preflight] Kiểm tra VIP %s chưa bị thiết bị nào khác chiếm dụng trong LAN...\n", state.Config.Spec.VIP)
		checkCmd := fmt.Sprintf("ping -c 1 -W 1 %s >/dev/null 2>&1 && echo IN_USE || echo FREE", state.Config.Spec.VIP)
		res, err := r.RunSudo(ctx, checkCmd)
		if err != nil {
			return fmt.Errorf("check VIP availability on %s: %w", cp1.Name, err)
		}
		if strings.TrimSpace(res.Stdout) == "IN_USE" {
			return fmt.Errorf("VIP %s is already responding on the network (possible IP conflict); choose an unused static IP in the same subnet", state.Config.Spec.VIP)
		}
	}

	return nil
}

func parseNodeFacts(nodeName, host, output string) (*api.NodeFacts, error) {
	kv := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}

	arch, err := normalizeLinuxArch(kv["ARCH"])
	if err != nil {
		return nil, err
	}

	cpus, _ := strconv.Atoi(kv["CPUS"])
	memMB, _ := strconv.Atoi(kv["MEM_MB"])

	return &api.NodeFacts{
		NodeName:         nodeName,
		Host:             host,
		OSID:             strings.ToLower(kv["OS_ID"]),
		OSVersion:        kv["OS_VER"],
		Arch:             arch,
		KernelVersion:    kv["KERNEL"],
		Hostname:         kv["HOSTNAME"],
		DefaultInterface: kv["IFACE"],
		CPUCores:         cpus,
		MemoryMB:         memMB,
		HasZstd:          kv["HAS_ZSTD"] == "1",
		HasGoldenImage:   kv["GOLDEN"] == "1",
	}, nil
}

func normalizeLinuxArch(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "x86_64", "amd64":
		return api.ArchAMD64, nil
	case "aarch64", "arm64":
		return api.ArchARM64, nil
	default:
		return "", fmt.Errorf("unsupported node CPU architecture %q", raw)
	}
}

func validateNodeFacts(node api.NodeConfig, facts *api.NodeFacts, cfg *api.ClusterConfig, manifest *api.BundleManifest) error {
	if facts.OSID != "ubuntu" {
		return fmt.Errorf("unsupported OS %q %s (vkube v0.1 targets Ubuntu 22.04 / 24.04 LTS)", facts.OSID, facts.OSVersion)
	}
	if cfg.Spec.Arch != api.ArchAuto && facts.Arch != cfg.Spec.Arch {
		return fmt.Errorf("node arch %q does not match cluster spec.arch %q", facts.Arch, cfg.Spec.Arch)
	}
	if manifest != nil && facts.Arch != manifest.Arch {
		return fmt.Errorf("node arch %q does not match Offline Bundle arch %q", facts.Arch, manifest.Arch)
	}
	if node.Role == api.RoleControlPlane {
		if facts.CPUCores < 2 {
			return fmt.Errorf("control-plane requires at least 2 vCPUs (detected %d)", facts.CPUCores)
		}
		// VM 2GB trên ARM64 thường có MemTotal khả dụng khoảng ~1600 MB sau khi kernel giữ chỗ.
		if facts.MemoryMB > 0 && facts.MemoryMB < 1500 {
			return fmt.Errorf("control-plane requires at least 1500 MB usable RAM (detected %d MB)", facts.MemoryMB)
		}
	}
	return nil
}

// Engine điều phối toàn bộ vòng đời triển khai, kiểm tra và gỡ bỏ cụm Kubernetes.
type Engine struct {
	Out io.Writer
}

// NewEngine khởi tạo Engine với luồng log out.
func NewEngine(out io.Writer) *Engine {
	if out == nil {
		out = io.Discard
	}
	return &Engine{Out: out}
}

// ClusterState giữ ngữ cảnh chạy xuyên suốt các giai đoạn của lệnh `vkube apply`.
type ClusterState struct {
	Config         *api.ClusterConfig
	BundlePath     string
	BundleManifest *api.BundleManifest
	Pool           *ssh.Pool
	Facts          map[string]*api.NodeFacts
}
