package bundle

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/versions"
)

const (
	ManifestFileName    = "bundle-manifest.json"
	BundleFormatVersion = "v1"
)

// Pack đóng gói toàn bộ thư mục stagingDir thành file Offline Bundle (.tar.zst)
// kèm bundle-manifest.json chứa danh sách SHA256 checksum của từng file.
func Pack(stagingDir, outputPath string, resolved *versions.ResolvedBOM) (*api.BundleManifest, error) {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}

	type stagedFile struct {
		localPath string
		posixPath string
		info      fs.FileInfo
		sha256    string
	}

	var files []stagedFile
	err := filepath.WalkDir(stagingDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(stagingDir, p)
		if err != nil {
			return err
		}
		posixPath := path.Clean(filepath.ToSlash(rel))
		if posixPath == ManifestFileName {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum, err := hashFileSHA256(p)
		if err != nil {
			return err
		}
		files = append(files, stagedFile{
			localPath: p,
			posixPath: posixPath,
			info:      info,
			sha256:    sum,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan staging directory: %w", err)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].posixPath < files[j].posixPath
	})

	manifest := &api.BundleManifest{
		FormatVersion:     BundleFormatVersion,
		ReleaseTag:        resolved.ReleaseTag,
		KubernetesVersion: resolved.KubernetesVersion,
		Arch:              resolved.Arch,
		BuiltAt:           time.Now().UTC(),
		Components:        resolved.Components,
		Files:             make([]api.BundleFile, 0, len(files)),
	}
	for _, f := range files {
		manifest.Files = append(manifest.Files, api.BundleFile{
			Path:   f.posixPath,
			SHA256: f.sha256,
			Size:   f.info.Size(),
		})
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal bundle manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	tmpOut := outputPath + ".tmp"
	outFile, err := os.Create(tmpOut)
	if err != nil {
		return nil, fmt.Errorf("create temp bundle file: %w", err)
	}
	cleanup := func() {
		_ = outFile.Close()
		_ = os.Remove(tmpOut)
	}

	zw, err := zstd.NewWriter(outFile, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("init zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)

	// Ghi bundle-manifest.json làm entry đầu tiên để Verify có thể đọc nhanh metadata.
	if err := tw.WriteHeader(&tar.Header{
		Name:    ManifestFileName,
		Mode:    0o644,
		Size:    int64(len(manifestBytes)),
		ModTime: manifest.BuiltAt,
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("write manifest header: %w", err)
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		cleanup()
		return nil, fmt.Errorf("write manifest body: %w", err)
	}

	for _, f := range files {
		mode := int64(f.info.Mode().Perm())
		if strings.HasPrefix(f.posixPath, "bin/") || strings.HasPrefix(f.posixPath, "cni-plugins/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:    f.posixPath,
			Mode:    mode,
			Size:    f.info.Size(),
			ModTime: f.info.ModTime().UTC(),
		}); err != nil {
			cleanup()
			return nil, fmt.Errorf("write tar header for %s: %w", f.posixPath, err)
		}

		src, err := os.Open(f.localPath)
		if err != nil {
			cleanup()
			return nil, err
		}
		if _, err := io.Copy(tw, src); err != nil {
			_ = src.Close()
			cleanup()
			return nil, fmt.Errorf("write tar body for %s: %w", f.posixPath, err)
		}
		_ = src.Close()
	}

	if err := tw.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("close tar writer: %w", err)
	}
	if err := zw.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("close zstd writer: %w", err)
	}
	if err := outFile.Close(); err != nil {
		_ = os.Remove(tmpOut)
		return nil, fmt.Errorf("close bundle file: %w", err)
	}
	if err := os.Rename(tmpOut, outputPath); err != nil {
		_ = os.Remove(tmpOut)
		return nil, fmt.Errorf("finalize bundle file %q: %w", outputPath, err)
	}

	return manifest, nil
}

// Verify kiểm tra toàn bộ cấu trúc, SHA256 checksum và kích thước của từng file trong Offline Bundle (.tar.zst).
func Verify(bundlePath string) (*api.BundleManifest, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("open bundle %q: %w", bundlePath, err)
	}
	defer f.Close()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("init zstd reader: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)

	var manifest *api.BundleManifest
	type actualEntry struct {
		sha256 string
		size   int64
	}
	actualFiles := make(map[string]actualEntry)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read bundle tar stream: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		entryName := path.Clean(hdr.Name)
		if entryName == ManifestFileName {
			raw, err := io.ReadAll(io.LimitReader(tr, 10<<20))
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", ManifestFileName, err)
			}
			var m api.BundleManifest
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("parse %s: %w", ManifestFileName, err)
			}
			manifest = &m
			continue
		}

		h := sha256.New()
		n, err := io.Copy(h, tr)
		if err != nil {
			return nil, fmt.Errorf("hash entry %s: %w", entryName, err)
		}
		actualFiles[entryName] = actualEntry{
			sha256: hex.EncodeToString(h.Sum(nil)),
			size:   n,
		}
	}

	if manifest == nil {
		return nil, fmt.Errorf("bundle is missing required %s", ManifestFileName)
	}
	if err := api.ValidateArch(manifest.Arch); err != nil {
		return nil, fmt.Errorf("bundle manifest has invalid arch: %w", err)
	}

	expectedSet := make(map[string]struct{}, len(manifest.Files))
	for _, exp := range manifest.Files {
		expectedSet[exp.Path] = struct{}{}
		act, ok := actualFiles[exp.Path]
		if !ok {
			return nil, fmt.Errorf("bundle missing expected file %q", exp.Path)
		}
		if act.size != exp.Size {
			return nil, fmt.Errorf("size mismatch for %q: expected %d, got %d", exp.Path, exp.Size, act.size)
		}
		if !strings.EqualFold(act.sha256, exp.SHA256) {
			return nil, fmt.Errorf("sha256 mismatch for %q: expected %s, got %s", exp.Path, exp.SHA256, act.sha256)
		}
	}

	for actPath := range actualFiles {
		if _, ok := expectedSet[actPath]; !ok {
			return nil, fmt.Errorf("bundle contains unexpected unverified file %q", actPath)
		}
	}

	return manifest, nil
}

func hashFileSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
