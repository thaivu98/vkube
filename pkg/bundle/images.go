package bundle

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/thaivu98/vkube/pkg/versions"
)

// ImageExporter định nghĩa giao diện kéo và đóng gói nhóm OCI images thành file tar tương thích containerd/ctr.
type ImageExporter interface {
	ExportGroup(ctx context.Context, group versions.ResolvedImageGroup, arch, destPath string, out io.Writer) error
}

// OCIImageExporter sử dụng Pure Go (go-containerregistry) để pull image theo kiến trúc linux/<arch>
// mà không cần cài đặt Docker daemon trên máy chạy CLI vkube.
type OCIImageExporter struct{}

// ExportGroup kéo toàn bộ OCI images trong group cho linux/<arch> và ghi vào 1 file tar duy nhất.
func (e *OCIImageExporter) ExportGroup(ctx context.Context, group versions.ResolvedImageGroup, arch, destPath string, out io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("mkdir for image group %s: %w", group.Name, err)
	}

	platform := v1.Platform{
		OS:           "linux",
		Architecture: arch,
	}

	tagToImage := make(map[name.Tag]v1.Image, len(group.Images))
	for _, imgRef := range group.Images {
		fmt.Fprintf(out, "  [image]    %-16s pulling %s (linux/%s)...\n", group.Name, imgRef, arch)
		tag, err := name.NewTag(imgRef)
		if err != nil {
			return fmt.Errorf("parse image reference %q: %w", imgRef, err)
		}
		img, err := remote.Image(
			tag,
			remote.WithContext(ctx),
			remote.WithPlatform(platform),
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
		)
		if err != nil {
			return fmt.Errorf("pull image %q for linux/%s: %w", imgRef, arch, err)
		}
		tagToImage[tag] = img
	}

	fmt.Fprintf(out, "  [image]    %-16s saving %d image(s) -> %s\n", group.Name, len(tagToImage), group.Dest)
	if err := tarball.MultiWriteToFile(destPath, tagToImage); err != nil {
		return fmt.Errorf("write image tarball %q: %w", destPath, err)
	}
	return nil
}
