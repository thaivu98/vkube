package api

import (
	"strings"
	"testing"
)

func TestParseClusterConfig_ValidSingleNodeWithDefaults(t *testing.T) {
	raw := `
metadata:
  name: lab-arm64
spec:
  arch: arm64
  ssh:
    password: "test-pass"
  nodes:
    - name: cp-1
      host: 192.0.2.10
      role: control-plane
      user: ubuntu
    - name: worker-1
      host: 192.0.2.11
      role: worker
      user: worker
`
	cfg, err := ParseClusterConfig([]byte(raw))
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}

	if cfg.APIVersion != DefaultAPIVersion {
		t.Errorf("expected apiVersion %q, got %q", DefaultAPIVersion, cfg.APIVersion)
	}
	if cfg.Spec.Networking.CNI != CNICalico {
		t.Errorf("expected default CNI %q, got %q", CNICalico, cfg.Spec.Networking.CNI)
	}
	if cfg.Spec.Networking.PodSubnet != DefaultPodSubnet {
		t.Errorf("expected default PodSubnet %q, got %q", DefaultPodSubnet, cfg.Spec.Networking.PodSubnet)
	}
	if len(cfg.ControlPlanes()) != 1 || len(cfg.Workers()) != 1 {
		t.Errorf("expected 1 control-plane and 1 worker, got %d and %d", len(cfg.ControlPlanes()), len(cfg.Workers()))
	}

	cpSSH := cfg.EffectiveSSH(cfg.ControlPlanes()[0])
	if cpSSH.User != "ubuntu" || cpSSH.Port != 22 || cpSSH.Password != "test-pass" || cpSSH.SudoPassword != "test-pass" {
		t.Errorf("unexpected control-plane EffectiveSSH: %+v", cpSSH)
	}
}

func TestParseClusterConfig_AutoArchDefault(t *testing.T) {
	raw := `
spec:
  ssh:
    password: "test-pass"
  nodes:
    - name: cp-1
      host: 192.0.2.10
      role: control-plane
`
	cfg, err := ParseClusterConfig([]byte(raw))
	if err != nil {
		t.Fatalf("expected valid config with omitted arch, got error: %v", err)
	}
	if cfg.Spec.Arch != ArchAuto {
		t.Errorf("expected default spec.arch %q, got %q", ArchAuto, cfg.Spec.Arch)
	}
}

func TestParseClusterConfig_ValidHA(t *testing.T) {
	raw := `
apiVersion: vkube.io/v1alpha1
kind: Cluster
metadata:
  name: prod-ha
spec:
  arch: amd64
  vip: 10.0.0.100
  ssh:
    user: ubuntu
    privateKeyPath: /home/ubuntu/.ssh/id_ed25519
  nodes:
    - name: cp-1
      host: 10.0.0.11
      role: control-plane
    - name: cp-2
      host: 10.0.0.12
      role: control-plane
    - name: cp-3
      host: 10.0.0.13
      role: control-plane
`
	cfg, err := ParseClusterConfig([]byte(raw))
	if err != nil {
		t.Fatalf("expected valid HA config, got error: %v", err)
	}
	if cfg.Spec.ControlPlaneEndpoint != "10.0.0.100:6443" {
		t.Errorf("expected auto-derived controlPlaneEndpoint 10.0.0.100:6443, got %q", cfg.Spec.ControlPlaneEndpoint)
	}
}

func TestParseClusterConfig_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unsupported arch",
			yaml: `
spec:
  arch: riscv64
  ssh: { password: "test-pass" }
  nodes:
    - { name: cp-1, host: 10.0.0.1, role: control-plane }
`,
			wantErr: "unsupported target architecture",
		},
		{
			name: "duplicate node name",
			yaml: `
spec:
  arch: arm64
  ssh: { password: "test-pass" }
  nodes:
    - { name: master, host: 10.0.0.1, role: control-plane }
    - { name: master, host: 10.0.0.2, role: worker }
`,
			wantErr: "is duplicated",
		},
		{
			name: "even control-plane count",
			yaml: `
spec:
  arch: amd64
  vip: 10.0.0.100
  ssh: { password: "test-pass" }
  nodes:
    - { name: cp-1, host: 10.0.0.1, role: control-plane }
    - { name: cp-2, host: 10.0.0.2, role: control-plane }
`,
			wantErr: "must be odd",
		},
		{
			name: "HA missing VIP or endpoint",
			yaml: `
spec:
  arch: amd64
  ssh: { password: "test-pass" }
  nodes:
    - { name: cp-1, host: 10.0.0.1, role: control-plane }
    - { name: cp-2, host: 10.0.0.2, role: control-plane }
    - { name: cp-3, host: 10.0.0.3, role: control-plane }
`,
			wantErr: "require spec.vip or spec.controlPlaneEndpoint",
		},
		{
			name: "missing ssh auth",
			yaml: `
spec:
  arch: arm64
  nodes:
    - { name: cp-1, host: 10.0.0.1, role: control-plane }
`,
			wantErr: "SSH authentication required",
		},
		{
			name: "invalid CIDR",
			yaml: `
spec:
  arch: amd64
  networking:
    podSubnet: not-a-cidr
  ssh: { password: "test-pass" }
  nodes:
    - { name: cp-1, host: 10.0.0.1, role: control-plane }
`,
			wantErr: "not a valid CIDR",
		},
		{
			name: "invalid taint effect",
			yaml: `
spec:
  arch: arm64
  ssh: { password: "test-pass" }
  nodes:
    - name: cp-1
      host: 10.0.0.1
      role: control-plane
    - name: worker-1
      host: 10.0.0.2
      role: worker
      taints:
        - key: dedicated
          value: gpu
          effect: InvalidEffect
`,
			wantErr: "must be one of NoSchedule, PreferNoSchedule, NoExecute",
		},
		{
			name: "VIP conflicts with node host",
			yaml: `
spec:
  arch: amd64
  vip: 10.0.0.1
  ssh: { password: "test-pass" }
  nodes:
    - { name: cp-1, host: 10.0.0.1, role: control-plane }
    - { name: cp-2, host: 10.0.0.2, role: control-plane }
    - { name: cp-3, host: 10.0.0.3, role: control-plane }
`,
			wantErr: "conflicts with an existing node host",
		},
		{
			name: "metalLB enabled without ipRange",
			yaml: `
spec:
  arch: arm64
  addons:
    metalLB:
      enabled: true
  ssh: { password: "test-pass" }
  nodes:
    - { name: cp-1, host: 10.0.0.1, role: control-plane }
`,
			wantErr: "spec.addons.metalLB.ipRange is required",
		},
		{
			name: "invalid DNS-1123 node name",
			yaml: `
spec:
  arch: arm64
  ssh: { password: "test-pass" }
  nodes:
    - { name: Worker_1, host: 10.0.0.1, role: control-plane }
`,
			wantErr: "must be a valid lowercase DNS-1123 name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClusterConfig([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}
