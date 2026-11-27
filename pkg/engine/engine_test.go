package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
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
	ipCmd := kubeletExtraArgsCmd("192.0.2.10", map[string]string{"max-pods": "150"})
	if !strings.Contains(ipCmd, `--node-ip=192.0.2.10`) || !strings.Contains(ipCmd, `--max-pods=150`) || !strings.Contains(ipCmd, `20-vkube-node-ip.conf`) {
		t.Errorf("expected systemd drop-in with --node-ip and --max-pods for IP host, got: %s", ipCmd)
	}
	dnsCmd := kubeletExtraArgsCmd("worker-1.internal.local", nil)
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
	writeEntry("backup-metadata.json", `{"cluster":"prod-ha","controlPlane":"cp-1"}`)
	_ = tw.Close()
	_ = gw.Close()
	_ = f.Close()

	if err := verifyBackupArchive(archivePath, "prod-ha", false); err != nil {
		t.Fatalf("expected valid backup archive for matching cluster, got error: %v", err)
	}
	if err := verifyBackupArchive(archivePath, "staging", false); err == nil || !strings.Contains(err.Error(), "backup belongs to cluster") {
		t.Fatalf("expected cluster mismatch error, got: %v", err)
	}
	if err := verifyBackupArchive(archivePath, "staging", true); err != nil {
		t.Fatalf("expected forceClusterMismatch to allow restore, got: %v", err)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestS3BackupUploadDownloadSigV4(t *testing.T) {
	stored := make(map[string][]byte)
	var lastAuth, lastHash string

	origFactory := s3TransportFactory
	t.Cleanup(func() { s3TransportFactory = origFactory })
	s3TransportFactory = func(_ bool) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			lastAuth = r.Header.Get("Authorization")
			lastHash = r.Header.Get("x-amz-content-sha256")
			switch r.Method {
			case http.MethodPut:
				body, _ := io.ReadAll(r.Body)
				stored[r.URL.Path] = body
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			case http.MethodGet:
				data, ok := stored[r.URL.Path]
				if !ok {
					return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
			default:
				return &http.Response{StatusCode: http.StatusMethodNotAllowed, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			}
		})
	}

	dir := t.TempDir()
	srcFile := filepath.Join(dir, "backup-test.tar.gz")
	payload := []byte("vkube-etcd-backup-payload-12345")
	if err := os.WriteFile(srcFile, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	s3Cfg := &api.S3BackupConfig{
		Endpoint:  "https://minio.internal:9000",
		Bucket:    "k8s-backups",
		Region:    "ap-southeast-1",
		AccessKey: "test-ak",
		SecretKey: "test-sk",
	}

	uri, err := s3UploadFile(context.Background(), s3Cfg, srcFile, "prod/backup-test.tar.gz")
	if err != nil {
		t.Fatalf("s3UploadFile failed: %v", err)
	}
	if uri != "s3://k8s-backups/prod/backup-test.tar.gz" {
		t.Fatalf("unexpected S3 URI: %q", uri)
	}
	if !strings.HasPrefix(lastAuth, "AWS4-HMAC-SHA256 Credential=test-ak/") || !strings.Contains(lastAuth, "/ap-southeast-1/s3/aws4_request") {
		t.Errorf("unexpected SigV4 Authorization header: %q", lastAuth)
	}
	if len(lastHash) != 64 {
		t.Errorf("expected 64-hex x-amz-content-sha256, got %q", lastHash)
	}

	dstFile := filepath.Join(dir, "downloaded.tar.gz")
	if err := s3DownloadFile(context.Background(), s3Cfg, "prod/backup-test.tar.gz", dstFile); err != nil {
		t.Fatalf("s3DownloadFile failed: %v", err)
	}
	got, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("downloaded payload mismatch: got %q, want %q", string(got), string(payload))
	}

	b, k, ok := parseS3URI("s3://k8s-backups/prod/backup-test.tar.gz")
	if !ok || b != "k8s-backups" || k != "prod/backup-test.tar.gz" {
		t.Fatalf("parseS3URI unexpected: b=%q k=%q ok=%v", b, k, ok)
	}
	rp, ok := parseSFTPURI("sftp://backup@10.0.0.50/data/backups/test.tar.gz")
	if !ok || rp != "/data/backups/test.tar.gz" {
		t.Fatalf("parseSFTPURI unexpected: rp=%q ok=%v", rp, ok)
	}
}

func TestPhase7Helpers_ProxyRetentionAndBGP(t *testing.T) {
	// 1. Proxy drop-in & EffectiveNoProxy
	cfg := &api.ClusterConfig{
		Spec: api.ClusterSpec{
			VIP:        "10.0.0.250",
			Networking: api.NetworkingConfig{PodSubnet: "10.244.0.0/16", ServiceSubnet: "10.96.0.0/12"},
			Proxy:      api.ProxyConfig{HTTPProxy: "http://proxy.corp:8080", HTTPSProxy: "http://proxy.corp:8080", NoProxy: ".corp.local"},
			Nodes:      []api.NodeConfig{{Name: "cp-1", Host: "10.0.0.1"}},
		},
	}
	dropin := renderContainerdProxyDropin(cfg)
	for _, want := range []string{`HTTP_PROXY=http://proxy.corp:8080`, `HTTPS_PROXY=http://proxy.corp:8080`, `10.244.0.0/16`, `10.0.0.250`, `.corp.local`} {
		if !strings.Contains(dropin, want) {
			t.Errorf("expected %q in containerd proxy drop-in:\n%s", want, dropin)
		}
	}

	// 2. pruneLocalBackups keeps newest N
	dir := t.TempDir()
	for _, name := range []string{
		"backup-prod-20260101-010000.tar.gz",
		"backup-prod-20260102-010000.tar.gz",
		"backup-prod-20260103-010000.tar.gz",
		"backup-prod-20260104-010000.tar.gz",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruned, err := pruneLocalBackups(dir, "backup-prod-", 2)
	if err != nil || len(pruned) != 2 {
		t.Fatalf("expected 2 pruned local backups, got %v (err=%v)", pruned, err)
	}

	// 3. s3PruneBackups via in-memory mock transport
	var deleted []string
	origFactory := s3TransportFactory
	t.Cleanup(func() { s3TransportFactory = origFactory })
	s3TransportFactory = func(_ bool) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.Method {
			case http.MethodGet:
				xmlBody := `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Contents><Key>vkube/backup-prod-20260101.tar.gz</Key></Contents><Contents><Key>vkube/backup-prod-20260102.tar.gz</Key></Contents><Contents><Key>vkube/backup-prod-20260103.tar.gz</Key></Contents></ListBucketResult>`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(xmlBody)), Header: make(http.Header)}, nil
			case http.MethodDelete:
				deleted = append(deleted, r.URL.Path)
				return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			default:
				return &http.Response{StatusCode: http.StatusMethodNotAllowed, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			}
		})
	}
	s3Pruned, err := s3PruneBackups(context.Background(), &api.S3BackupConfig{
		Endpoint: "https://minio.internal:9000", Bucket: "b", AccessKey: "ak", SecretKey: "sk",
	}, "vkube/backup-prod-", 2)
	if err != nil || len(s3Pruned) != 1 || s3Pruned[0] != "vkube/backup-prod-20260101.tar.gz" || len(deleted) != 1 {
		t.Fatalf("unexpected s3PruneBackups result: pruned=%v deleted=%v err=%v", s3Pruned, deleted, err)
	}

	// 4. MetalLB BGP manifest rendering
	bgpManifest := renderMetalLBPoolManifest(api.MetalLBAddon{
		Enabled: true,
		IPRange: "10.0.10.100-10.0.10.120",
		BGP:     &api.BGPConfig{Enabled: true, LocalASN: 64512, PeerASN: 64500, PeerIP: "10.0.0.1"},
	})
	for _, want := range []string{"kind: BGPPeer", "myASN: 64512", "peerASN: 64500", "peerAddress: 10.0.0.1", "kind: BGPAdvertisement"} {
		if !strings.Contains(bgpManifest, want) {
			t.Errorf("expected %q in MetalLB BGP manifest:\n%s", want, bgpManifest)
		}
	}
}

func TestPhase8And9Helpers_GoldenImageAndUpgradePath(t *testing.T) {
	// 1. Golden Image strict tag + arch matching (không bao giờ bỏ qua mù quáng khi thiếu tag hoặc lệch arch)
	if !isGoldenImageMatched(&api.NodeFacts{HasGoldenImage: true, Arch: api.ArchARM64, GoldenImageTag: "v1.36.7-vkube.1", GoldenImageArch: api.ArchARM64}, "v1.36.7-vkube.1") {
		t.Error("expected exact golden image tag+arch match")
	}
	if isGoldenImageMatched(&api.NodeFacts{HasGoldenImage: true, Arch: api.ArchARM64, GoldenImageTag: ""}, "v1.36.7-vkube.1") {
		t.Error("expected untagged golden image to be REJECTED (fail-closed)")
	}
	if isGoldenImageMatched(&api.NodeFacts{HasGoldenImage: true, Arch: api.ArchARM64, GoldenImageTag: "v1.36.7-vkube.1", GoldenImageArch: api.ArchAMD64}, "v1.36.7-vkube.1") {
		t.Error("expected mismatched golden image arch to be REJECTED")
	}
	if isGoldenImageMatched(&api.NodeFacts{HasGoldenImage: true, Arch: api.ArchARM64, GoldenImageTag: "v1.32.2-vkube.1"}, "v1.36.7-vkube.1") {
		t.Error("expected mismatched golden image tag to return false")
	}

	// 2. Duplicate machine-id detection in preflight
	dupCfg := &api.ClusterConfig{
		Metadata: api.ClusterMetadata{Name: "dup-mid"},
		Spec: api.ClusterSpec{
			Arch: api.ArchAuto,
			Nodes: []api.NodeConfig{
				{Name: "cp-1", Host: "10.0.0.1", Role: api.RoleControlPlane},
				{Name: "worker-1", Host: "10.0.0.2", Role: api.RoleWorker},
			},
		},
	}
	probeDup := "OS_ID=ubuntu\nOS_VER=24.04\nARCH=aarch64\nKERNEL=6.8.0\nHOSTNAME=n\nIFACE=ens160\nCPUS=2\nMEM_MB=2048\nHAS_ZSTD=1\nGOLDEN=0\nMACHINE_ID=same-cloned-id-123\n"
	dupPool := ssh.NewPool(map[string]ssh.Runner{
		"cp-1":     &fakeNodeRunner{name: "cp-1", host: "10.0.0.1", output: probeDup},
		"worker-1": &fakeNodeRunner{name: "worker-1", host: "10.0.0.2", output: probeDup},
	}, io.Discard)
	dupState := &ClusterState{Config: dupCfg, OnlineMode: true, Pool: dupPool, Facts: make(map[string]*api.NodeFacts)}
	if err := NewEngine(io.Discard).RunPreflight(context.Background(), dupState); err == nil || !strings.Contains(err.Error(), "duplicate /etc/machine-id") {
		t.Fatalf("expected duplicate machine-id error, got: %v", err)
	}

	// 3. Multi-NIC nodeIP separation
	multiNIC := []api.NodeConfig{{Name: "cp-1", Host: "10.10.0.1", NodeIP: "10.20.0.1", Role: api.RoleControlPlane}}
	if h := buildHostsBlock(multiNIC); !strings.Contains(h, "10.20.0.1 cp-1") {
		t.Errorf("expected EffectiveNodeIP 10.20.0.1 in hosts block:\n%s", h)
	}
	if ic := buildEtcdInitialCluster(multiNIC); ic != "cp-1=https://10.20.0.1:2380" {
		t.Errorf("expected EffectiveNodeIP in etcd initial cluster, got: %q", ic)
	}

	// 2. validateUpgradePath
	for _, tc := range []struct {
		cur, target string
		wantErr     bool
	}{
		{"v1.35.11", "v1.36.7", false},
		{"v1.36.7", "v1.36.8", false},
		{"v1.36.7", "v1.36.7", true},  // same version
		{"v1.36.7", "v1.35.11", true}, // downgrade
		{"v1.34.5", "v1.36.7", true},  // skip 2 minors
	} {
		err := validateUpgradePath(tc.cur, tc.target)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateUpgradePath(%s -> %s): err=%v, wantErr=%v", tc.cur, tc.target, err, tc.wantErr)
		}
	}
}
