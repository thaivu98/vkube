package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/versions"
)

const maxDownloadRetries = 3

func downloadArtifact(ctx context.Context, client *http.Client, art versions.ResolvedArtifact, stagingDir string, out io.Writer) error {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}

	tmpFile, err := os.CreateTemp("", "vkube-dl-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", art.Name, err)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	defer os.Remove(tmpPath)

	fmt.Fprintf(out, "  [download] %-16s %s\n", art.Name, art.URL)
	actualSHA, err := fetchFileWithRetry(ctx, client, art.URL, tmpPath)
	if err != nil {
		return fmt.Errorf("download artifact %q: %w", art.Name, err)
	}

	if art.SHA256 != "" {
		if !strings.EqualFold(actualSHA, art.SHA256) {
			return fmt.Errorf("pinned sha256 mismatch for %q: expected %s, got %s", art.Name, art.SHA256, actualSHA)
		}
		fmt.Fprintf(out, "  [verify]   %-16s pinned sha256 OK (%s...)\n", art.Name, actualSHA[:12])
	} else if art.SHA256URL != "" {
		expectedSHA, err := fetchSHA256(ctx, client, art.SHA256URL, path.Base(art.URL))
		if err != nil {
			return fmt.Errorf("fetch sha256 for %q: %w", art.Name, err)
		}
		if !strings.EqualFold(actualSHA, expectedSHA) {
			return fmt.Errorf("sha256 mismatch for %q: expected %s, got %s", art.Name, expectedSHA, actualSHA)
		}
		fmt.Fprintf(out, "  [verify]   %-16s sha256 OK (%s...)\n", art.Name, actualSHA[:12])
	}

	destLocal := filepath.Join(stagingDir, filepath.FromSlash(art.Dest))

	switch art.Type {
	case versions.ArtifactTypeBinary:
		return copyFile(tmpPath, destLocal, 0o755)
	case versions.ArtifactTypeManifest:
		return copyFile(tmpPath, destLocal, 0o644)
	case versions.ArtifactTypeTarball:
		return extractTarGz(tmpPath, destLocal, art.Dest)
	default:
		return fmt.Errorf("unsupported artifact type %q for %s", art.Type, art.Name)
	}
}

func fetchFileWithRetry(ctx context.Context, client *http.Client, url, destPath string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxDownloadRetries; attempt++ {
		sha, err := fetchFileOnce(ctx, client, url, destPath)
		if err == nil {
			return sha, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if attempt < maxDownloadRetries {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
	}
	return "", lastErr
}

func fetchFileOnce(ctx context.Context, client *http.Client, url, destPath string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fetchSHA256(ctx context.Context, client *http.Client, shaURL, targetFileName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, shaURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, shaURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}

	var firstValid string
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || len(fields[0]) != 64 {
			continue
		}
		hash := strings.ToLower(fields[0])
		if firstValid == "" {
			firstValid = hash
		}
		if len(fields) >= 2 && targetFileName != "" {
			fname := strings.TrimPrefix(fields[1], "*")
			if path.Base(fname) == targetFileName {
				return hash, nil
			}
		}
	}
	if firstValid != "" {
		return firstValid, nil
	}
	return "", fmt.Errorf("invalid sha256 payload from %s: %q", shaURL, strings.TrimSpace(string(body)))
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// extractTarGz giải nén file .tar.gz / .tgz bằng Pure Go với bảo vệ Zip-Slip.
// Nếu entry bên trong archive có tiền tố trùng với destPrefix (ví dụ "bin/containerd" giải nén vào "bin"),
// tiền tố thừa sẽ tự động được cắt gọn để tránh tạo ra đường dẫn "bin/bin/containerd".
func extractTarGz(archivePath, destDir, destPrefix string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", destDir, err)
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip %s: %w", archivePath, err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	cleanPrefix := strings.Trim(path.Clean(destPrefix), "/")
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		entryPosix := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if cleanPrefix != "" && strings.HasPrefix(entryPosix, cleanPrefix+"/") {
			entryPosix = strings.TrimPrefix(entryPosix, cleanPrefix+"/")
		}
		if entryPosix == "" || entryPosix == "." {
			continue
		}

		targetPath := filepath.Join(destDir, filepath.FromSlash(entryPosix))
		targetAbs, err := filepath.Abs(targetPath)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(targetAbs, destAbs+string(os.PathSeparator)) && targetAbs != destAbs {
			return fmt.Errorf("illegal archive entry path %q", hdr.Name)
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode)
		if mode == 0 {
			mode = 0o755
		}
		outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(outFile, tr); err != nil {
			_ = outFile.Close()
			return err
		}
		if err := outFile.Close(); err != nil {
			return err
		}
	}
	return nil
}
