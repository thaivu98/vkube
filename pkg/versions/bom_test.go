package versions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thaivu98/vkube/pkg/api"
)

func TestLoadDefaultAndResolveMultiArch(t *testing.T) {
	bom, err := Load("")
	if err != nil {
		t.Fatalf("Load default BOM failed: %v", err)
	}

	for _, arch := range []string{api.ArchAMD64, api.ArchARM64} {
		t.Run(arch, func(t *testing.T) {
			resolved, err := bom.Resolve(arch)
			if err != nil {
				t.Fatalf("Resolve(%s) failed: %v", arch, err)
			}
			if resolved.Arch != arch {
				t.Errorf("expected arch %q, got %q", arch, resolved.Arch)
			}
			if resolved.KubernetesVersion == "" {
				t.Fatal("expected non-empty KubernetesVersion")
			}

			foundArchURL := false
			for _, a := range resolved.Artifacts {
				if strings.Contains(a.URL, "{{") {
					t.Errorf("artifact %q URL still contains unrendered template: %s", a.Name, a.URL)
				}
				if strings.Contains(a.URL, arch) {
					foundArchURL = true
				}
				if a.Type == ArtifactTypeManifest && !sha256HexRe.MatchString(a.SHA256) {
					t.Errorf("manifest artifact %q must have a valid 64-hex pinned sha256, got %q", a.Name, a.SHA256)
				}
			}
			if !foundArchURL {
				t.Errorf("expected at least one artifact URL to contain arch %q", arch)
			}

			for _, g := range resolved.ImageGroups {
				for _, img := range g.Images {
					if strings.Contains(img, "{{") {
						t.Errorf("image group %q contains unrendered template: %s", g.Name, img)
					}
				}
			}
		})
	}
}

func TestLoadFromManifestOverride(t *testing.T) {
	// Giả lập user xuất file versions.yaml mặc định và sửa đúng 1 dòng phiên bản Kubernetes.
	customYAML := strings.Replace(string(DefaultBOMBytes()), "kubernetes: v1.32.2", "kubernetes: v1.32.5", 1)
	customYAML = strings.Replace(customYAML, "releaseTag: v1.32.2-vkube.1", "releaseTag: v1.32.5-vkube.1", 1)

	manifestPath := filepath.Join(t.TempDir(), "custom-versions.yaml")
	if err := os.WriteFile(manifestPath, []byte(customYAML), 0o600); err != nil {
		t.Fatalf("write temp manifest: %v", err)
	}

	bom, err := Load(manifestPath)
	if err != nil {
		t.Fatalf("Load(%s) failed: %v", manifestPath, err)
	}
	resolved, err := bom.Resolve(api.ArchARM64)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if resolved.KubernetesVersion != "v1.32.5" {
		t.Fatalf("expected overridden k8s version v1.32.5, got %q", resolved.KubernetesVersion)
	}

	var kubeadmURL string
	for _, a := range resolved.Artifacts {
		if a.Name == "kubeadm" {
			kubeadmURL = a.URL
			break
		}
	}
	wantURL := "https://dl.k8s.io/release/v1.32.5/bin/linux/arm64/kubeadm"
	if kubeadmURL != wantURL {
		t.Errorf("expected kubeadm URL %q, got %q", wantURL, kubeadmURL)
	}

	foundAPIServer := false
	for _, g := range resolved.ImageGroups {
		for _, img := range g.Images {
			if img == "registry.k8s.io/kube-apiserver:v1.32.5" {
				foundAPIServer = true
			}
		}
	}
	if !foundAPIServer {
		t.Errorf("expected kube-apiserver image to use overridden version v1.32.5")
	}
}

func TestResolve_InvalidArch(t *testing.T) {
	bom, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault failed: %v", err)
	}
	if _, err := bom.Resolve("s390x"); err == nil {
		t.Fatal("expected error for unsupported arch s390x, got nil")
	}
}

func TestParse_InvalidSHA256Rejected(t *testing.T) {
	badYAML := `
releaseTag: v1.32.2-vkube.1
components:
  kubernetes: v1.32.2
artifacts:
  - name: bad-manifest
    component: kubernetes
    type: manifest
    url: https://example.com/a.yaml
    sha256: "not-a-valid-hash; rm -rf /"
    dest: manifests/a.yaml
`
	if _, err := Parse([]byte(badYAML)); err == nil || !strings.Contains(err.Error(), "invalid sha256") {
		t.Fatalf("expected invalid sha256 error, got: %v", err)
	}
}
