package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/versions"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newMockHTTPClient(routes map[string][]byte) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			data, ok := routes[r.URL.Path]
			if !ok {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
					Header:     make(http.Header),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(data)),
				Header:     make(http.Header),
			}, nil
		}),
	}
}

type fakeImageExporter struct{}

func (f *fakeImageExporter) ExportGroup(_ context.Context, group versions.ResolvedImageGroup, arch, destPath string, _ io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	payload := fmt.Sprintf("mock-oci-tar:%s:%s:%s", group.Name, arch, strings.Join(group.Images, ","))
	return os.WriteFile(destPath, []byte(payload), 0o644)
}

func makeMockTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(content)),
		}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestBuilderBuildAndVerify(t *testing.T) {
	kubeadmData := []byte("#!/bin/sh\necho kubeadm\n")
	containerdTarGz := makeMockTarGz(t, map[string]string{
		"bin/containerd": "mock-containerd-bin",
		"bin/ctr":        "mock-ctr-bin",
	})
	calicoManifest := []byte("apiVersion: v1\nkind: ConfigMap\n")

	client := newMockHTTPClient(map[string][]byte{
		"/kubeadm":                  kubeadmData,
		"/kubeadm.sha256":           []byte(sha256Hex(kubeadmData) + "\n"),
		"/containerd.tar.gz":        containerdTarGz,
		"/containerd.tar.gz.sha256": []byte(sha256Hex(containerdTarGz) + "  containerd.tar.gz\n"),
		"/calico.yaml":              calicoManifest,
	})
	baseURL := "https://mock.vkube.test"

	resolved := &versions.ResolvedBOM{
		ReleaseTag:        "v1.32.2-vkube.1",
		KubernetesVersion: "v1.32.2",
		Arch:              api.ArchARM64,
		Components:        map[string]string{"kubernetes": "v1.32.2", "containerd": "1.7.25", "calico": "v3.29.2"},
		Artifacts: []versions.ResolvedArtifact{
			{
				Name:      "kubeadm",
				Component: "kubernetes",
				Version:   "v1.32.2",
				Type:      versions.ArtifactTypeBinary,
				URL:       baseURL + "/kubeadm",
				SHA256URL: baseURL + "/kubeadm.sha256",
				Dest:      "bin/kubeadm",
			},
			{
				Name:      "containerd",
				Component: "containerd",
				Version:   "1.7.25",
				Type:      versions.ArtifactTypeTarball,
				URL:       baseURL + "/containerd.tar.gz",
				SHA256URL: baseURL + "/containerd.tar.gz.sha256",
				Dest:      "bin",
			},
			{
				Name:      "calico-manifest",
				Component: "calico",
				Version:   "v3.29.2",
				Type:      versions.ArtifactTypeManifest,
				URL:       baseURL + "/calico.yaml",
				Dest:      "manifests/calico.yaml",
			},
		},
		ImageGroups: []versions.ResolvedImageGroup{
			{
				Name:   "kubernetes",
				Dest:   "images/kubernetes.tar",
				Images: []string{"registry.k8s.io/kube-apiserver:v1.32.2"},
			},
		},
	}

	outPath := filepath.Join(t.TempDir(), "vkube-test-arm64.tar.zst")
	builder := &Builder{
		HTTPClient:    client,
		ImageExporter: &fakeImageExporter{},
		Out:           io.Discard,
		Concurrency:   2,
	}

	manifest, err := builder.Build(context.Background(), resolved, outPath)
	if err != nil {
		t.Fatalf("Builder.Build failed: %v", err)
	}

	verified, err := Verify(outPath)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if verified.Arch != api.ArchARM64 || verified.ReleaseTag != manifest.ReleaseTag {
		t.Fatalf("unexpected verified manifest: %+v", verified)
	}

	// Đảm bảo containerd đã được cắt tiền tố "bin/" thừa, nằm đúng tại "bin/containerd" (không phải "bin/bin/containerd")
	expectedPaths := map[string]bool{
		"bin/kubeadm":           false,
		"bin/containerd":        false,
		"bin/ctr":               false,
		"manifests/calico.yaml": false,
		"images/kubernetes.tar": false,
	}
	for _, bf := range verified.Files {
		if _, ok := expectedPaths[bf.Path]; ok {
			expectedPaths[bf.Path] = true
		}
	}
	for p, found := range expectedPaths {
		if !found {
			t.Errorf("expected file %q in bundle manifest, not found", p)
		}
	}
}

func TestVerify_DetectsTamperedFile(t *testing.T) {
	// Tạo bundle hợp lệ rồi ghi đè 1 file bên trong với nội dung bị sửa đổi để kiểm tra Verify phát hiện lỗi SHA256.
	tmpDir := t.TempDir()
	staging := filepath.Join(tmpDir, "staging")
	if err := os.MkdirAll(filepath.Join(staging, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "bin", "kubeadm"), []byte("legit-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	resolved := &versions.ResolvedBOM{
		ReleaseTag:        "v1.32.2-vkube.1",
		KubernetesVersion: "v1.32.2",
		Arch:              api.ArchAMD64,
		Components:        map[string]string{"kubernetes": "v1.32.2"},
	}

	goodPath := filepath.Join(tmpDir, "good.tar.zst")
	manifest, err := Pack(staging, goodPath, resolved)
	if err != nil {
		t.Fatalf("Pack failed: %v", err)
	}

	// Tạo bundle bị sửa nội dung file bin/kubeadm nhưng giữ nguyên manifest cũ
	tamperedPath := filepath.Join(tmpDir, "tampered.tar.zst")
	f, err := os.Create(tamperedPath)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)

	// Đọc lại bundle-manifest.json từ goodPath
	goodFile, _ := os.Open(goodPath)
	goodZR, _ := zstd.NewReader(goodFile)
	goodTR := tar.NewReader(goodZR)
	hdr, _ := goodTR.Next()
	manifestBytes, _ := io.ReadAll(goodTR)
	goodZR.Close()
	goodFile.Close()

	_ = tw.WriteHeader(&tar.Header{Name: hdr.Name, Mode: 0o644, Size: int64(len(manifestBytes))})
	_, _ = tw.Write(manifestBytes)

	corrupted := []byte("evil-binary!")
	_ = tw.WriteHeader(&tar.Header{Name: "bin/kubeadm", Mode: 0o755, Size: int64(len(corrupted))})
	_, _ = tw.Write(corrupted)

	_ = tw.Close()
	_ = zw.Close()
	_ = f.Close()

	if _, err := Verify(tamperedPath); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch for tampered bundle (manifest had %d files), got: %v", len(manifest.Files), err)
	}
}

func TestDownloadArtifact_BadSHA256Fails(t *testing.T) {
	client := newMockHTTPClient(map[string][]byte{
		"/bin":        []byte("payload"),
		"/bin.sha256": []byte(strings.Repeat("0", 64)),
	})

	art := versions.ResolvedArtifact{
		Name:      "test-bin",
		Type:      versions.ArtifactTypeBinary,
		URL:       "https://mock.vkube.test/bin",
		SHA256URL: "https://mock.vkube.test/bin.sha256",
		Dest:      "bin/test-bin",
	}
	err := downloadArtifact(context.Background(), client, art, t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch error, got: %v", err)
	}
}
