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
	"github.com/thaivu98/vkube/pkg/versions"
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
	return e.runPreflightNodes(ctx, state, state.Config.Spec.Nodes, true)
}

func (e *Engine) runPreflightNodes(ctx context.Context, state *ClusterState, nodes []api.NodeConfig, checkVIP bool) error {
	fmt.Fprintf(e.Out, "==> [1/6] Preflight: Thu thập NodeFacts (1 lần duy nhất) & kiểm tra %d node(s)...\n", len(nodes))

	var mu sync.Mutex
	err := state.Pool.RunParallel(ctx, nodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
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

	// Đảm bảo toàn bộ các node trong cụm có cùng kiến trúc CPU (chống lẫn amd64/arm64 ở chế độ Online).
	var expectedArch string
	for _, n := range nodes {
		f := state.Facts[n.Name]
		if f == nil {
			continue
		}
		if expectedArch == "" {
			expectedArch = f.Arch
			continue
		}
		if f.Arch != expectedArch {
			return fmt.Errorf("mixed CPU architectures detected: node %q is %s while cluster expects %s", n.Name, f.Arch, expectedArch)
		}
	}

	// Kiểm tra trên toàn bộ nodes: chưa cài cụm & các port bắt buộc còn trống (CP: 6443/2379/2380/10250, Worker: 10250).
	if err := state.Pool.RunParallel(ctx, nodes, func(ctx context.Context, node api.NodeConfig, r ssh.Runner) error {
		ports := "10250"
		if node.Role == api.RoleControlPlane {
			ports = "6443 2379 2380 10250"
		}
		checkScript := fmt.Sprintf(`set -euo pipefail
if [ -f /etc/kubernetes/kubelet.conf ] || [ -f /etc/kubernetes/admin.conf ]; then
  echo "ERR:CLUSTER_EXISTS"
  exit 0
fi
if command -v ss >/dev/null 2>&1; then
  for p in %s; do
    if ss -lnt "( sport = :$p )" 2>/dev/null | grep -q ":$p"; then
      echo "ERR:PORT_IN_USE:$p"
      exit 0
    fi
  done
fi
echo "OK"
`, ports)
		if res, err := r.RunSudo(ctx, checkScript); err == nil {
			out := strings.TrimSpace(res.Stdout)
			if out == "ERR:CLUSTER_EXISTS" {
				return fmt.Errorf("node appears to already belong to a Kubernetes cluster (/etc/kubernetes/kubelet.conf or admin.conf exists); run 'vkube reset' first")
			}
			if strings.HasPrefix(out, "ERR:PORT_IN_USE:") {
				return fmt.Errorf("required port %s is already in use", strings.TrimPrefix(out, "ERR:PORT_IN_USE:"))
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if !checkVIP || state.Config.Spec.VIP == "" {
		return nil
	}

	// Kiểm tra chống trùng IP VIP trên mạng LAN thực tế từ Primary Control Plane.
	cp1 := state.Config.ControlPlanes()[0]
	cp1Runner, err := state.Pool.Get(cp1.Name)
	if err != nil {
		return err
	}
	iface := state.Config.Spec.VIPInterface
	if iface == "" && state.Facts[cp1.Name] != nil {
		iface = state.Facts[cp1.Name].DefaultInterface
	}
	fmt.Fprintf(e.Out, "  [preflight] Kiểm tra VIP %s chưa bị thiết bị nào khác chiếm dụng trong LAN...\n", state.Config.Spec.VIP)
	// Chỉ dùng iputils arping -D khi xác nhận đúng bản iputils; nếu rc khác 0/1 (ví dụ lỗi 2) thì fallback ping.
	checkCmd := fmt.Sprintf(`if command -v arping >/dev/null 2>&1 && arping -V 2>&1 | grep -qi iputils && [ -n %q ]; then
  arping -D -I %q -c 2 -w 2 %s >/dev/null 2>&1
  rc=$?
  if [ "$rc" -eq 0 ]; then
    echo FREE
  elif [ "$rc" -eq 1 ]; then
    echo IN_USE
  else
    ping -c 1 -W 1 %s >/dev/null 2>&1 && echo IN_USE || echo FREE
  fi
else
  ping -c 1 -W 1 %s >/dev/null 2>&1 && echo IN_USE || echo FREE
fi`, iface, iface, state.Config.Spec.VIP, state.Config.Spec.VIP, state.Config.Spec.VIP)
	res, err := cp1Runner.RunSudo(ctx, checkCmd)
	if err != nil {
		return fmt.Errorf("check VIP availability on %s: %w", cp1.Name, err)
	}
	if strings.TrimSpace(res.Stdout) == "IN_USE" {
		return fmt.Errorf("VIP %s is already responding on the network (possible IP conflict); choose an unused static IP in the same subnet", state.Config.Spec.VIP)
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

func isRHELFamily(osID string) bool {
	switch strings.ToLower(strings.TrimSpace(osID)) {
	case "rocky", "almalinux", "rhel":
		return true
	default:
		return false
	}
}

func parseMajorVersion(ver string) (int, bool) {
	majorStr := ver
	if s, _, ok := strings.Cut(ver, "."); ok {
		majorStr = s
	}
	v, err := strconv.Atoi(strings.TrimSpace(majorStr))
	if err != nil {
		return 0, false
	}
	return v, true
}

func validateNodeFacts(node api.NodeConfig, facts *api.NodeFacts, cfg *api.ClusterConfig, manifest *api.BundleManifest) error {
	switch facts.OSID {
	case "ubuntu":
		if major, ok := parseMajorVersion(facts.OSVersion); ok && major < 22 {
			return fmt.Errorf("unsupported Ubuntu version %s (minimum required: 22.04)", facts.OSVersion)
		}
	case "rocky", "almalinux", "rhel":
		if major, ok := parseMajorVersion(facts.OSVersion); ok && major < 8 {
			return fmt.Errorf("unsupported %s version %s (minimum required: 8)", facts.OSID, facts.OSVersion)
		}
	default:
		return fmt.Errorf("unsupported OS %q %s (supported: Ubuntu >= 22.04, Rocky/AlmaLinux/RHEL >= 8)", facts.OSID, facts.OSVersion)
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

// syncWriter bảo vệ luồng ghi Out an toàn khi nhiều goroutine ghi log song song.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Engine điều phối toàn bộ vòng đời triển khai, kiểm tra và gỡ bỏ cụm Kubernetes.
type Engine struct {
	Out io.Writer
}

// NewEngine khởi tạo Engine với luồng log out được bảo vệ thread-safe.
func NewEngine(out io.Writer) *Engine {
	if out == nil {
		out = io.Discard
	}
	return &Engine{Out: &syncWriter{w: out}}
}

// ClusterState giữ ngữ cảnh chạy xuyên suốt các giai đoạn của lệnh `vkube apply`.
type ClusterState struct {
	Config         *api.ClusterConfig
	OnlineMode     bool
	FastSync       bool
	BundlePath     string
	BundleManifest *api.BundleManifest
	ResolvedBOM    *versions.ResolvedBOM
	Pool           *ssh.Pool
	Facts          map[string]*api.NodeFacts
}
