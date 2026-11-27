package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
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
func (f *fakeNodeRunner) UploadFile(_ context.Context, _, _ string) error   { return nil }
func (f *fakeNodeRunner) DownloadFile(_ context.Context, _, _ string) error { return nil }
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

func TestScaleHelpers(t *testing.T) {
	all := []api.NodeConfig{
		{Name: "cp-1", Host: "10.0.0.1", Role: api.RoleControlPlane},
		{Name: "cp-2", Host: "10.0.0.2", Role: api.RoleControlPlane},
		{Name: "worker-1", Host: "10.0.0.3", Role: api.RoleWorker},
	}
	existing := map[string]struct{}{"cp-1": {}}

	// Auto-detect missing nodes
	added, err := selectNodesToAdd(all, existing, "")
	if err != nil || len(added) != 2 {
		t.Fatalf("expected 2 new nodes, got %v (err=%v)", added, err)
	}
	// Request specific new node
	one, err := selectNodesToAdd(all, existing, "worker-1")
	if err != nil || len(one) != 1 || one[0].Name != "worker-1" {
		t.Fatalf("expected worker-1, got %v (err=%v)", one, err)
	}
	// Reject already-existing node
	if _, err := selectNodesToAdd(all, existing, "cp-1"); err == nil {
		t.Fatal("expected error for already-existing node cp-1")
	}

	// pickActiveControlPlane protects last remaining CP
	singleCP := &api.ClusterConfig{Spec: api.ClusterSpec{Nodes: []api.NodeConfig{all[0]}}}
	if _, err := pickActiveControlPlane(singleCP, "cp-1"); err == nil {
		t.Fatal("expected refusal when removing last control-plane")
	}
	multiCP := &api.ClusterConfig{Spec: api.ClusterSpec{Nodes: all}}
	active, err := pickActiveControlPlane(multiCP, "cp-1")
	if err != nil || active.Name != "cp-2" {
		t.Fatalf("expected active CP cp-2, got %+v (err=%v)", active, err)
	}

	// parseUploadCertsKey
	key := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	parsedKey, err := parseUploadCertsKey("[upload-certs] Storing the certificates...\nUsing certificate key:\n" + key + "\n")
	if err != nil || parsedKey != key {
		t.Fatalf("parseUploadCertsKey got %q (err=%v)", parsedKey, err)
	}

	// parseEtcdMemberID
	memberOut := "8e9e05c52164694d, started, cp-1, https://10.0.0.1:2380, https://10.0.0.1:2379, false\nabcdef1234567890, started, cp-2, https://10.0.0.2:2380, https://10.0.0.2:2379, false\n"
	if id := parseEtcdMemberID(memberOut, "cp-2", "10.0.0.2"); id != "abcdef1234567890" {
		t.Fatalf("expected etcd member ID abcdef1234567890, got %q", id)
	}
}

func TestBackupAndRestoreHelpers(t *testing.T) {
	cps := []api.NodeConfig{
		{Name: "cp-1", Host: "10.0.0.1", Role: api.RoleControlPlane},
		{Name: "cp-2", Host: "10.0.0.2", Role: api.RoleControlPlane},
		{Name: "cp-3", Host: "10.0.0.3", Role: api.RoleControlPlane},
	}
	got := buildEtcdInitialCluster(cps)
	want := "cp-1=https://10.0.0.1:2380,cp-2=https://10.0.0.2:2380,cp-3=https://10.0.0.3:2380"
	if got != want {
		t.Fatalf("buildEtcdInitialCluster = %q, want %q", got, want)
	}

	// Tạo archive backup hợp lệ bằng Pure Go và kiểm tra verifyBackupArchive
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	writeEntry := func(name, content string) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
	}
	writeEntry("etcd-snapshot.db", "fake-etcd-db-bytes")
	writeEntry("pki/ca.crt", "fake-cert")
	_ = tw.Close()
	_ = gw.Close()
	_ = f.Close()

	if err := verifyBackupArchive(archivePath); err != nil {
		t.Fatalf("expected valid backup archive, got error: %v", err)
	}
}

func TestValidateNodeFacts_RHELAndRockySupport(t *testing.T) {
	cfg := &api.ClusterConfig{Spec: api.ClusterSpec{Arch: api.ArchAuto}}
	node := api.NodeConfig{Name: "cp-1", Role: api.RoleControlPlane}

	for _, tc := range []struct {
		osID    string
		osVer   string
		wantErr bool
	}{
		{"ubuntu", "24.04", false},
		{"ubuntu", "20.04", true},
		{"rocky", "9.4", false},
		{"rocky", "8.10", false},
		{"rocky", "7.9", true},
		{"almalinux", "9.4", false},
		{"rhel", "8.9", false},
		{"alpine", "3.20", true},
	} {
		facts := &api.NodeFacts{OSID: tc.osID, OSVersion: tc.osVer, Arch: api.ArchARM64, CPUCores: 2, MemoryMB: 2048}
		err := validateNodeFacts(node, facts, cfg, nil)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateNodeFacts(%s %s): err=%v, wantErr=%v", tc.osID, tc.osVer, err, tc.wantErr)
		}
	}

	rhelCP := buildRHELPrepScript(api.RoleControlPlane)
	for _, want := range []string{"SELINUX=permissive", "6443/tcp", "2379-2380/tcp", "4240/tcp", "8472/udp"} {
		if !strings.Contains(rhelCP, want) {
			t.Errorf("expected %q in RHEL prep script:\n%s", want, rhelCP)
		}
	}
	resetScript := buildNodeResetScript("10.0.0.250")
	for _, want := range []string{"10.0.0.250/32", "CILIUM_", "cilium_host", "lxc"} {
		if !strings.Contains(resetScript, want) {
			t.Errorf("expected %q in reset script:\n%s", want, resetScript)
		}
	}
}
