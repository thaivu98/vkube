package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"github.com/thaivu98/vkube/pkg/api"
	gossh "golang.org/x/crypto/ssh"
)

const defaultDialTimeout = 15 * time.Second

// ExecResult lưu kết quả đầu ra stdout/stderr của một lệnh chạy trên Target Node.
type ExecResult struct {
	Stdout string
	Stderr string
}

// Runner định nghĩa giao diện thực thi lệnh và truyền file tới một Target Node Linux.
// Tách thành interface giúp tầng pkg/engine dễ dàng viết unit test mà không cần VM thật.
type Runner interface {
	NodeName() string
	Host() string
	Run(ctx context.Context, command string) (ExecResult, error)
	RunSudo(ctx context.Context, command string) (ExecResult, error)
	UploadFile(ctx context.Context, localPath, remotePosixPath string) error
	WriteFile(ctx context.Context, content []byte, remotePosixPath string, mode os.FileMode) error
	Close() error
}

// Client là cài đặt Pure Go của Runner sử dụng golang.org/x/crypto/ssh và pkg/sftp.
type Client struct {
	nodeName string
	host     string
	cfg      api.SSHConfig
	conn     *gossh.Client
}

// Dial kết nối SSH tới một Target Node bằng Pure Go (hỗ trợ cả Private Key và Password).
func Dial(ctx context.Context, nodeName, host string, sshCfg api.SSHConfig) (*Client, error) {
	authMethods, err := buildAuthMethods(sshCfg)
	if err != nil {
		return nil, fmt.Errorf("node %s (%s): %w", nodeName, host, err)
	}

	port := sshCfg.Port
	if port == 0 {
		port = api.DefaultSSHPort
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	clientCfg := &gossh.ClientConfig{
		User:            sshCfg.User,
		Auth:            authMethods,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), // Lab/On-prem provisioning ban đầu; v0.5 bổ sung known_hosts strict mode.
		Timeout:         defaultDialTimeout,
	}

	dialer := &net.Dialer{Timeout: defaultDialTimeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh tcp dial %s (%s): %w", nodeName, addr, err)
	}

	c, chans, reqs, err := gossh.NewClientConn(rawConn, addr, clientCfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("ssh handshake %s (%s): %w", nodeName, addr, err)
	}

	return &Client{
		nodeName: nodeName,
		host:     host,
		cfg:      sshCfg,
		conn:     gossh.NewClient(c, chans, reqs),
	}, nil
}

func (c *Client) NodeName() string { return c.nodeName }
func (c *Client) Host() string     { return c.host }

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Run thực thi một lệnh shell trên Target Node với quyền user SSH hiện tại.
func (c *Client) Run(ctx context.Context, command string) (ExecResult, error) {
	return c.exec(ctx, command, nil)
}

// RunSudo thực thi lệnh với quyền root. Nếu user không phải root, tự động dùng `sudo -S -p ''`
// và truyền mật khẩu qua stdin (không bao giờ lộ mật khẩu trong tham số tiến trình).
func (c *Client) RunSudo(ctx context.Context, command string) (ExecResult, error) {
	wrapped, stdin := buildSudoCommand(c.cfg.User, c.cfg.SudoPassword, command)
	return c.exec(ctx, wrapped, stdin)
}

func (c *Client) exec(ctx context.Context, command string, stdin io.Reader) (ExecResult, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return ExecResult{}, fmt.Errorf("create ssh session on %s: %w", c.nodeName, err)
	}
	defer sess.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	sess.Stdout = &stdoutBuf
	sess.Stderr = &stderrBuf
	if stdin != nil {
		sess.Stdin = stdin
	}

	done := make(chan error, 1)
	go func() {
		done <- sess.Run(command)
	}()

	select {
	case <-ctx.Done():
		_ = sess.Signal(gossh.SIGKILL)
		_ = sess.Close()
		return ExecResult{Stdout: stdoutBuf.String(), Stderr: stderrBuf.String()}, ctx.Err()
	case err := <-done:
		res := ExecResult{
			Stdout: stdoutBuf.String(),
			Stderr: stderrBuf.String(),
		}
		if err != nil {
			return res, fmt.Errorf("node %s command failed: %w (stderr: %s)", c.nodeName, err, strings.TrimSpace(res.Stderr))
		}
		return res, nil
	}
}

// UploadFile tải một file cục bộ từ máy Deployer lên đường dẫn POSIX trên Target Node qua Pure Go SFTP.
func (c *Client) UploadFile(ctx context.Context, localPath, remotePosixPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local file %q: %w", localPath, err)
	}
	defer src.Close()

	sftpClient, err := sftp.NewClient(c.conn)
	if err != nil {
		return fmt.Errorf("open sftp session on %s: %w", c.nodeName, err)
	}
	defer sftpClient.Close()

	remoteDir := path.Dir(remotePosixPath)
	if remoteDir != "" && remoteDir != "." && remoteDir != "/" {
		if err := sftpClient.MkdirAll(remoteDir); err != nil {
			return fmt.Errorf("sftp mkdir %s on %s: %w", remoteDir, c.nodeName, err)
		}
	}

	dst, err := sftpClient.Create(remotePosixPath)
	if err != nil {
		return fmt.Errorf("sftp create %s on %s: %w", remotePosixPath, c.nodeName, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("sftp upload %s to %s: %w", remotePosixPath, c.nodeName, err)
	}
	return dst.Close()
}

// WriteFile ghi trực tiếp một mảng byte xuống đường dẫn POSIX trên Target Node qua SFTP.
func (c *Client) WriteFile(ctx context.Context, content []byte, remotePosixPath string, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sftpClient, err := sftp.NewClient(c.conn)
	if err != nil {
		return fmt.Errorf("open sftp session on %s: %w", c.nodeName, err)
	}
	defer sftpClient.Close()

	remoteDir := path.Dir(remotePosixPath)
	if remoteDir != "" && remoteDir != "." && remoteDir != "/" {
		if err := sftpClient.MkdirAll(remoteDir); err != nil {
			return fmt.Errorf("sftp mkdir %s on %s: %w", remoteDir, c.nodeName, err)
		}
	}

	dst, err := sftpClient.Create(remotePosixPath)
	if err != nil {
		return fmt.Errorf("sftp create %s on %s: %w", remotePosixPath, c.nodeName, err)
	}
	if _, err := dst.Write(content); err != nil {
		_ = dst.Close()
		return fmt.Errorf("sftp write %s on %s: %w", remotePosixPath, c.nodeName, err)
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if mode != 0 {
		_ = sftpClient.Chmod(remotePosixPath, mode)
	}
	return nil
}

func buildAuthMethods(cfg api.SSHConfig) ([]gossh.AuthMethod, error) {
	var methods []gossh.AuthMethod

	if cfg.PrivateKeyPath != "" {
		keyPath, err := expandHomePath(cfg.PrivateKeyPath)
		if err != nil {
			return nil, err
		}
		keyBytes, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read ssh private key %q: %w", keyPath, err)
		}
		signer, err := gossh.ParsePrivateKey(keyBytes)
		if err != nil {
			return nil, fmt.Errorf("parse ssh private key %q: %w", keyPath, err)
		}
		methods = append(methods, gossh.PublicKeys(signer))
	}

	if cfg.Password != "" {
		methods = append(methods,
			gossh.Password(cfg.Password),
			gossh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = cfg.Password
				}
				return answers, nil
			}),
		)
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH auth method configured (provide privateKeyPath or password)")
	}
	return methods, nil
}

// expandHomePath mở rộng tiền tố "~/" thành thư mục Home hiện tại (chạy chuẩn trên Linux/macOS/Windows).
func expandHomePath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home dir: %w", err)
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

// buildSudoCommand bọc lệnh an toàn bằng single-quote POSIX và cấp sudo password qua stdin nếu cần.
func buildSudoCommand(user, sudoPassword, command string) (string, io.Reader) {
	quoted := shellQuote(command)
	if user == "root" {
		return "bash -lc " + quoted, nil
	}
	if sudoPassword != "" {
		return "sudo -S -p '' bash -lc " + quoted, strings.NewReader(sudoPassword + "\n")
	}
	return "sudo -n bash -lc " + quoted, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
