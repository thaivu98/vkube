package engine

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

type fakeNodeRunner struct {
	name   string
	host   string
	output string
}

func (f *fakeNodeRunner) NodeName() string { return f.name }
func (f *fakeNodeRunner) Host() string     { return f.host }
func (f *fakeNodeRunner) Run(_ context.Context, _ string) (ssh.ExecResult, error) {
	return ssh.ExecResult{Stdout: f.output}, nil
}
func (f *fakeNodeRunner) RunSudo(_ context.Context, _ string) (ssh.ExecResult, error) {
	return ssh.ExecResult{Stdout: f.output}, nil
}
func (f *fakeNodeRunner) UploadFile(_ context.Context, _, _ string) error { return nil }
func (f *fakeNodeRunner) WriteFile(_ context.Context, _ []byte, _ string, _ os.FileMode) error {
	return nil
}
func (f *fakeNodeRunner) Close() error { return nil }

func TestParseNodeFacts(t *testing.T) {
	raw := `OS_ID=ubuntu
OS_VER=24.04
ARCH=aarch64
KERNEL=6.8.0-45-generic
HOSTNAME=master
IFACE=ens160
CPUS=4
MEM_MB=4096
HAS_ZSTD=1
GOLDEN=0
`
	facts, err := parseNodeFacts("cp-1", "192.0.2.10", raw)
	if err != nil {
		t.Fatalf("parseNodeFacts failed: %v", err)
	}
	if facts.OSID != "ubuntu" || facts.OSVersion != "24.04" || facts.Arch != api.ArchARM64 {
		t.Errorf("unexpected OS/Arch facts: %+v", facts)
	}
	if facts.DefaultInterface != "ens160" || facts.CPUCores != 4 || facts.MemoryMB != 4096 || !facts.HasZstd {
		t.Errorf("unexpected hardware/tool facts: %+v", facts)
	}
}

func TestRunPreflight_CachesNodeFactsAndDetectsArchMismatch(t *testing.T) {
	cfg := &api.ClusterConfig{
		Metadata: api.ClusterMetadata{Name: "test"},
		Spec: api.ClusterSpec{
			Arch: api.ArchAuto,
			Nodes: []api.NodeConfig{
				{Name: "cp-1", Host: "192.0.2.10", Role: api.RoleControlPlane},
			},
		},
	}

	probeARM64 := "OS_ID=ubuntu\nOS_VER=24.04\nARCH=aarch64\nKERNEL=6.8.0\nHOSTNAME=cp-1\nIFACE=ens160\nCPUS=2\nMEM_MB=2048\nHAS_ZSTD=1\nGOLDEN=0\n"
	pool := ssh.NewPool(map[string]ssh.Runner{
		"cp-1": &fakeNodeRunner{name: "cp-1", host: "192.0.2.10", output: probeARM64},
	}, io.Discard)

	eng := NewEngine(io.Discard)

	// 1. Khớp bundle ARM64 -> Pass & cache vào state.Facts
	state := &ClusterState{
		Config:         cfg,
		BundleManifest: &api.BundleManifest{Arch: api.ArchARM64},
		Pool:           pool,
		Facts:          make(map[string]*api.NodeFacts),
	}
	if err := eng.RunPreflight(context.Background(), state); err != nil {
		t.Fatalf("expected preflight pass, got: %v", err)
	}
	if state.Facts["cp-1"] == nil || state.Facts["cp-1"].Arch != api.ArchARM64 {
		t.Fatalf("expected cached NodeFacts for cp-1, got: %+v", state.Facts["cp-1"])
	}

	// 2. Đưa nhầm bundle AMD64 cho node ARM64 -> Phải báo lỗi ngay ở preflight
	badState := &ClusterState{
		Config:         cfg,
		BundleManifest: &api.BundleManifest{Arch: api.ArchAMD64},
		Pool:           pool,
		Facts:          make(map[string]*api.NodeFacts),
	}
	err := eng.RunPreflight(context.Background(), badState)
	if err == nil || !strings.Contains(err.Error(), "does not match Offline Bundle arch") {
		t.Fatalf("expected bundle arch mismatch error, got: %v", err)
	}

	// 3. Cụm lẫn amd64 + arm64 ở chế độ Online (BundleManifest == nil) -> Phải báo lỗi mixed arch ngay ở preflight
	mixedCfg := &api.ClusterConfig{
		Metadata: api.ClusterMetadata{Name: "mixed"},
		Spec: api.ClusterSpec{
			Arch: api.ArchAuto,
			Nodes: []api.NodeConfig{
				{Name: "cp-1", Host: "192.0.2.10", Role: api.RoleControlPlane},
				{Name: "worker-1", Host: "192.0.2.11", Role: api.RoleWorker},
			},
		},
	}
	probeAMD64 := strings.Replace(probeARM64, "ARCH=aarch64", "ARCH=x86_64", 1)
	mixedPool := ssh.NewPool(map[string]ssh.Runner{
		"cp-1":     &fakeNodeRunner{name: "cp-1", host: "192.0.2.10", output: probeARM64},
		"worker-1": &fakeNodeRunner{name: "worker-1", host: "192.0.2.11", output: probeAMD64},
	}, io.Discard)
	mixedState := &ClusterState{
		Config:     mixedCfg,
		OnlineMode: true,
		Pool:       mixedPool,
		Facts:      make(map[string]*api.NodeFacts),
	}
	err = eng.RunPreflight(context.Background(), mixedState)
	if err == nil || !strings.Contains(err.Error(), "mixed CPU architectures detected") {
		t.Fatalf("expected mixed CPU arch error, got: %v", err)
	}
}

func TestBuildHostsBlock(t *testing.T) {
	block := buildHostsBlock([]api.NodeConfig{
		{Name: "cp-1", Host: "192.0.2.10"},
		{Name: "worker-1", Host: "192.0.2.11"},
	})
	if !strings.Contains(block, "192.0.2.10 cp-1") || !strings.Contains(block, "192.0.2.11 worker-1") {
		t.Fatalf("unexpected hosts block:\n%s", block)
	}
}

func TestRenderRegistryHostsToml(t *testing.T) {
	toml := renderRegistryHostsToml(api.RegistryMirror{
		Host:     "harbor.internal",
		Endpoint: "https://10.0.0.50:8443",
		Insecure: true,
	}, "/etc/containerd/certs.d/harbor.internal/ca.crt")
	for _, want := range []string{
		`server = "https://harbor.internal"`,
		`[host."https://10.0.0.50:8443"]`,
		`skip_verify = true`,
		`ca = "/etc/containerd/certs.d/harbor.internal/ca.crt"`,
	} {
		if !strings.Contains(toml, want) {
			t.Errorf("expected %q in rendered hosts.toml:\n%s", want, toml)
		}
	}
}

func TestKubeletExtraArgsCmd_IPvsDNS(t *testing.T) {
	ipCmd := kubeletExtraArgsCmd("192.0.2.10")
	if !strings.Contains(ipCmd, `--node-ip=192.0.2.10`) || !strings.Contains(ipCmd, `20-vkube-node-ip.conf`) {
		t.Errorf("expected systemd drop-in with --node-ip for IP host, got: %s", ipCmd)
	}
	dnsCmd := kubeletExtraArgsCmd("worker-1.internal.local")
	if strings.Contains(dnsCmd, `--node-ip`) {
		t.Errorf("DNS host must not set --node-ip, got: %s", dnsCmd)
	}
}

func TestParseJoinCommand(t *testing.T) {
	raw := "kubeadm join 192.0.2.100:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:1234abcd\n"
	ep, tok, hash, err := parseJoinCommand(raw)
	if err != nil {
		t.Fatalf("parseJoinCommand failed: %v", err)
	}
	if ep != "192.0.2.100:6443" || tok != "abcdef.0123456789abcdef" || hash != "sha256:1234abcd" {
		t.Fatalf("unexpected parsed values: ep=%q tok=%q hash=%q", ep, tok, hash)
	}
}

func TestContainerdConfigPatchCmd(t *testing.T) {
	cmd := containerdConfigPatchCmd("registry.k8s.io/pause:3.10.2")
	for _, want := range []string{
		"SystemdCgroup = true",
		`sandbox_image = "registry.k8s.io/pause:3.10.2"`,
		`sandbox = `,
		`registry.k8s.io/pause:3.10.2`,
		`config_path = "/etc/containerd/certs.d"`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("expected %q in containerdConfigPatchCmd:\n%s", want, cmd)
		}
	}
}
