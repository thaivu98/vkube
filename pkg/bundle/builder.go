package bundle

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/versions"
)

// Builder điều phối quy trình tải artifacts, pull OCI images và đóng gói Offline Bundle (.tar.zst).
type Builder struct {
	HTTPClient    *http.Client
	ImageExporter ImageExporter
	Out           io.Writer
	Concurrency   int
}

// NewBuilder khởi tạo Builder mặc định dùng Pure Go HTTP client và OCIImageExporter.
func NewBuilder(out io.Writer) *Builder {
	if out == nil {
		out = io.Discard
	}
	return &Builder{
		HTTPClient:    &http.Client{Timeout: 10 * time.Minute},
		ImageExporter: &OCIImageExporter{},
		Out:           out,
		Concurrency:   4,
	}
}

// Build thực thi trọn gói quy trình tạo Offline Bundle cho kiến trúc đã chọn trong ResolvedBOM.
func (b *Builder) Build(ctx context.Context, resolved *versions.ResolvedBOM, outputPath string) (*api.BundleManifest, error) {
	out := b.Out
	if out == nil {
		out = io.Discard
	}
	exporter := b.ImageExporter
	if exporter == nil {
		exporter = &OCIImageExporter{}
	}
	concurrency := b.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	stagingDir, err := os.MkdirTemp("", "vkube-bundle-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	start := time.Now()
	fmt.Fprintf(out, "==> [1/3] Tải %d artifacts cho linux/%s...\n", len(resolved.Artifacts), resolved.Arch)
	if err := b.downloadAllArtifacts(ctx, resolved.Artifacts, stagingDir, concurrency, out); err != nil {
		return nil, err
	}

	fmt.Fprintf(out, "==> [2/3] Pull & export %d nhóm OCI images cho linux/%s (Pure Go)...\n", len(resolved.ImageGroups), resolved.Arch)
	for _, group := range resolved.ImageGroups {
		destPath := filepath.Join(stagingDir, filepath.FromSlash(group.Dest))
		if err := exporter.ExportGroup(ctx, group, resolved.Arch, destPath, out); err != nil {
			return nil, err
		}
	}

	fmt.Fprintf(out, "==> [3/3] Đóng gói Offline Bundle (.tar.zst) -> %s\n", outputPath)
	manifest, err := Pack(stagingDir, outputPath, resolved)
	if err != nil {
		return nil, err
	}

	stat, _ := os.Stat(outputPath)
	var sizeMB float64
	if stat != nil {
		sizeMB = float64(stat.Size()) / (1024 * 1024)
	}
	fmt.Fprintf(out, "Hoàn tất Offline Bundle: %s (%.1f MB, %d files, mất %s)\n",
		outputPath, sizeMB, len(manifest.Files), time.Since(start).Round(time.Millisecond))
	return manifest, nil
}

func (b *Builder) downloadAllArtifacts(ctx context.Context, artifacts []versions.ResolvedArtifact, stagingDir string, concurrency int, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, concurrency)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)

	syncOut := &lockedWriter{w: out}

	for _, art := range artifacts {
		wg.Add(1)
		go func(a versions.ResolvedArtifact) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if err := downloadArtifact(ctx, b.HTTPClient, a, stagingDir, syncOut); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}(art)
	}

	wg.Wait()
	return firstErr
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}
