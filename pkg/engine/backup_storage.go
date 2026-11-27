package engine

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/thaivu98/vkube/pkg/api"
	"github.com/thaivu98/vkube/pkg/ssh"
)

// s3ObjectURL xây dựng URL đích và canonical URI cho S3 / MinIO / Ceph RGW (mặc định path-style).
func s3ObjectURL(cfg *api.S3BackupConfig, objectKey string) (rawURL, canonicalURI, hostHeader string, err error) {
	ep := strings.TrimSpace(cfg.Endpoint)
	if ep == "" {
		region := cfg.Region
		if region == "" {
			region = "us-east-1"
		}
		ep = fmt.Sprintf("https://s3.%s.amazonaws.com", region)
	}
	u, err := url.Parse(ep)
	if err != nil {
		return "", "", "", fmt.Errorf("parse S3 endpoint %q: %w", ep, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", "", "", fmt.Errorf("invalid S3 endpoint %q (must include scheme, e.g. https://minio.internal:9000)", ep)
	}

	key := strings.TrimLeft(objectKey, "/")
	usePathStyle := true
	if cfg.UsePathStyle != nil {
		usePathStyle = *cfg.UsePathStyle
	}

	if usePathStyle {
		u.Path = "/" + path.Join(cfg.Bucket, key)
		canonicalURI = awsEncodePath("/" + cfg.Bucket + "/" + key)
		hostHeader = u.Host
	} else {
		u.Host = cfg.Bucket + "." + u.Host
		u.Path = "/" + key
		canonicalURI = awsEncodePath("/" + key)
		hostHeader = u.Host
	}
	return u.String(), canonicalURI, hostHeader, nil
}

// signS3Request ký HTTP request theo chuẩn AWS Signature Version 4 bằng 100% Pure Go (không cần AWS SDK).
func signS3Request(req *http.Request, cfg *api.S3BackupConfig, canonicalURI, hostHeader, payloadHash string, now time.Time) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	req.Host = hostHeader
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", hostHeader, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+cfg.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		cfg.AccessKey, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
}

var s3TransportFactory = func(insecure bool) http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // người dùng tự bật cho MinIO nội bộ self-signed
	}
	return tr
}

func newS3HTTPClient(insecure bool) *http.Client {
	return &http.Client{Transport: s3TransportFactory(insecure), Timeout: 15 * time.Minute}
}

// s3UploadFile đẩy file cục bộ lên S3 / MinIO / Ceph RGW qua HTTP PUT có chữ ký AWS SigV4.
func s3UploadFile(ctx context.Context, cfg *api.S3BackupConfig, localPath, objectKey string) (string, error) {
	payloadHash, size, err := fileSHA256(localPath)
	if err != nil {
		return "", fmt.Errorf("hash backup file for S3 upload: %w", err)
	}
	rawURL, canonicalURI, hostHeader, err := s3ObjectURL(cfg, objectKey)
	if err != nil {
		return "", err
	}

	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, f)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")
	signS3Request(req, cfg, canonicalURI, hostHeader, payloadHash, time.Now())

	resp, err := newS3HTTPClient(cfg.InsecureSkipVerify).Do(req)
	if err != nil {
		return "", fmt.Errorf("S3 PUT %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("S3 PUT %s failed (HTTP %d): %s", rawURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Sprintf("s3://%s/%s", cfg.Bucket, strings.TrimLeft(objectKey, "/")), nil
}

// s3DownloadFile tải file backup từ S3 / MinIO / Ceph RGW về máy Deployer (quyền 0600).
func s3DownloadFile(ctx context.Context, cfg *api.S3BackupConfig, objectKey, localPath string) error {
	rawURL, canonicalURI, hostHeader, err := s3ObjectURL(cfg, objectKey)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	emptyHash := sha256Hex(nil)
	signS3Request(req, cfg, canonicalURI, hostHeader, emptyHash, time.Now())

	resp, err := newS3HTTPClient(cfg.InsecureSkipVerify).Do(req)
	if err != nil {
		return fmt.Errorf("S3 GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("S3 GET %s failed (HTTP %d): %s", rawURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if dir := filepath.Dir(localPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	dst, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		_ = dst.Close()
		_ = os.Remove(localPath)
		return err
	}
	return dst.Close()
}

// sftpUploadBackup đẩy file backup lên máy chủ Backup / NAS qua Pure Go SFTP.
func sftpUploadBackup(ctx context.Context, cfg *api.SFTPBackupConfig, localPath, fileName string) (string, error) {
	client, err := ssh.Dial(ctx, "backup-sftp", cfg.Host, api.SSHConfig{
		User:                  cfg.User,
		Port:                  cfg.Port,
		PrivateKeyPath:        cfg.PrivateKeyPath,
		PrivateKeyPassphrase:  cfg.PrivateKeyPassphrase,
		Password:              cfg.Password,
		InsecureIgnoreHostKey: cfg.InsecureIgnoreHostKey,
	})
	if err != nil {
		return "", fmt.Errorf("connect SFTP backup server %s: %w", cfg.Host, err)
	}
	defer client.Close()

	remotePath := path.Join(cfg.RemoteDir, fileName)
	if err := client.UploadFile(ctx, localPath, remotePath); err != nil {
		return "", fmt.Errorf("upload backup via SFTP to %s:%s: %w", cfg.Host, remotePath, err)
	}
	return fmt.Sprintf("sftp://%s@%s%s", cfg.User, cfg.Host, remotePath), nil
}

// sftpDownloadBackup tải file backup từ máy chủ Backup / NAS về máy Deployer qua Pure Go SFTP.
func sftpDownloadBackup(ctx context.Context, cfg *api.SFTPBackupConfig, remotePath, localPath string) error {
	client, err := ssh.Dial(ctx, "backup-sftp", cfg.Host, api.SSHConfig{
		User:                  cfg.User,
		Port:                  cfg.Port,
		PrivateKeyPath:        cfg.PrivateKeyPath,
		PrivateKeyPassphrase:  cfg.PrivateKeyPassphrase,
		Password:              cfg.Password,
		InsecureIgnoreHostKey: cfg.InsecureIgnoreHostKey,
	})
	if err != nil {
		return fmt.Errorf("connect SFTP backup server %s: %w", cfg.Host, err)
	}
	defer client.Close()

	return client.DownloadFile(ctx, remotePath, localPath)
}

// parseS3URI tách "s3://<bucket>/<key>" thành bucket và key.
func parseS3URI(uri string) (bucket, key string, ok bool) {
	if !strings.HasPrefix(uri, "s3://") {
		return "", "", false
	}
	rest := strings.TrimPrefix(uri, "s3://")
	b, k, found := strings.Cut(rest, "/")
	if !found || b == "" || k == "" {
		return "", "", false
	}
	return b, k, true
}

// parseSFTPURI tách "sftp://[user@host]/path/to/file.tar.gz" thành đường dẫn POSIX trên server SFTP.
func parseSFTPURI(uri string) (remotePath string, ok bool) {
	if !strings.HasPrefix(uri, "sftp://") {
		return "", false
	}
	u, err := url.Parse(uri)
	if err != nil || u.Path == "" {
		return "", false
	}
	return u.Path, true
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func awsEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsURIEncode(s)
	}
	return strings.Join(segs, "/")
}

func awsURIEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
