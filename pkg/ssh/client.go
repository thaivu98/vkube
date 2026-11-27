package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"github.com/thaivu98/vkube/pkg/api"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

var knownHostsMu sync.Mutex

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
	DownloadFile(ctx context.Context, remotePosixPath, localPath string) error
	WriteFile(ctx context.Context, content []byte, remotePosixPath string, mode os.FileMode) error
	Close() error
}

// Client là cài đặt Pure Go của Runner sử dụng golang.org/x/crypto/ssh và pkg/sftp (hỗ trợ cả SSH Bastion / Jump Host).
type Client struct {
	nodeName      string
	host          string
	cfg           api.SSHConfig
	conn          *gossh.Client
	agentConn     io.Closer
	bastionClient *gossh.Client
	bastionAgent  io.Closer
}

// Dial kết nối SSH tới một Target Node bằng Pure Go (hỗ trợ Private Key, ssh-agent, Password và SSH Bastion / ProxyJump).
func Dial(ctx context.Context, nodeName, host string, sshCfg api.SSHConfig) (*Client, error) {
	authMethods, agentConn, err := buildAuthMethods(sshCfg)
	if err != nil {
		return nil, fmt.Errorf("node %s (%s): %w", nodeName, host, err)
	}
	closeAgent := func() {
		if agentConn != nil {
			_ = agentConn.Close()
		}
	}

	port := sshCfg.Port
	if port == 0 {
		port = api.DefaultSSHPort
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	hostKeyCB, err := buildHostKeyCallback(sshCfg)
	if err != nil {
		closeAgent()
		return nil, fmt.Errorf("node %s (%s): %w", nodeName, host, err)
	}

	clientCfg := &gossh.ClientConfig{
		User:            sshCfg.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCB,
		Timeout:         defaultDialTimeout,
	}

	var (
		rawConn       net.Conn
		bastionClient *gossh.Client
		bastionAgent  io.Closer
	)
	closeBastion := func() {
		if bastionClient != nil {
			_ = bastionClient.Close()
		}
		if bastionAgent != nil {
			_ = bastionAgent.Close()
		}
	}

	// Nếu có cấu hình Bastion/Jump Host, mở tunnel SSH xuyên qua Bastion bằng 100% Pure Go
	if sshCfg.Bastion != nil && strings.TrimSpace(sshCfg.Bastion.Host) != "" {
		b := sshCfg.Bastion
		bCfg := api.SSHConfig{
			User:                  b.User,
			Port:                  b.Port,
			PrivateKeyPath:        b.PrivateKeyPath,
			PrivateKeyPassphrase:  b.PrivateKeyPassphrase,
			Password:              b.Password,
			KnownHostsPath:        b.KnownHostsPath,
			InsecureIgnoreHostKey: b.InsecureIgnoreHostKey || sshCfg.InsecureIgnoreHostKey,
		}
		if bCfg.Port == 0 {
			bCfg.Port = api.DefaultSSHPort
		}
		if bCfg.User == "" {
			bCfg.User = sshCfg.User
		}
		bAuth, bAgent, err := buildAuthMethods(bCfg)
		if err != nil {
			closeAgent()
			return nil, fmt.Errorf("bastion %s: %w", b.Host, err)
		}
		bastionAgent = bAgent

		bHostKeyCB, err := buildHostKeyCallback(bCfg)
		if err != nil {
			closeBastion()
			closeAgent()
			return nil, fmt.Errorf("bastion %s host key callback: %w", b.Host, err)
		}
		bAddr := net.JoinHostPort(b.Host, fmt.Sprintf("%d", bCfg.Port))
		dialer := &net.Dialer{Timeout: defaultDialTimeout}
		bRaw, err := dialer.DialContext(ctx, "tcp", bAddr)
		if err != nil {
			closeBastion()
			closeAgent()
			return nil, fmt.Errorf("ssh tcp dial bastion %s: %w", bAddr, err)
		}
		bc, bChans, bReqs, err := gossh.NewClientConn(bRaw, bAddr, &gossh.ClientConfig{
			User:            bCfg.User,
			Auth:            bAuth,
			HostKeyCallback: bHostKeyCB,
			Timeout:         defaultDialTimeout,
		})
		if err != nil {
			_ = bRaw.Close()
			closeBastion()
			closeAgent()
			return nil, fmt.Errorf("ssh handshake bastion %s: %w", bAddr, err)
		}
		bastionClient = gossh.NewClient(bc, bChans, bReqs)
		rawConn, err = bastionClient.Dial("tcp", addr)
		if err != nil {
			closeBastion()
			closeAgent()
			return nil, fmt.Errorf("bastion %s dial target %s (%s): %w", bAddr, nodeName, addr, err)
		}
	} else {
		dialer := &net.Dialer{Timeout: defaultDialTimeout}
		rawConn, err = dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			closeAgent()
			return nil, fmt.Errorf("ssh tcp dial %s (%s): %w", nodeName, addr, err)
		}
	}

	c, chans, reqs, err := gossh.NewClientConn(rawConn, addr, clientCfg)
	if err != nil {
		_ = rawConn.Close()
		closeBastion()
		closeAgent()
		return nil, fmt.Errorf("ssh handshake %s (%s): %w", nodeName, addr, err)
	}

	return &Client{
		nodeName:      nodeName,
		host:          host,
		cfg:           sshCfg,
		conn:          gossh.NewClient(c, chans, reqs),
		agentConn:     agentConn,
		bastionClient: bastionClient,
		bastionAgent:  bastionAgent,
	}, nil
}

func (c *Client) NodeName() string { return c.nodeName }
func (c *Client) Host() string     { return c.host }

func (c *Client) Close() error {
	var firstErr error
	if c.conn != nil {
		if err := c.conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.conn = nil
	}
	if c.agentConn != nil {
		_ = c.agentConn.Close()
		c.agentConn = nil
	}
	if c.bastionClient != nil {
		if err := c.bastionClient.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.bastionClient = nil
	}
	if c.bastionAgent != nil {
		_ = c.bastionAgent.Close()
		c.bastionAgent = nil
	}
	return firstErr
}

// Run thực thi một lệnh shell trên Target Node với quyền user SSH hiện tại.
func (c *Client) Run(ctx context.Context, command string) (ExecResult, error) {
	return c.exec(ctx, command, nil)
}

// RunSudo thực thi lệnh với quyền root. Nếu user không phải root, tự động dùng `sudo -S -p ”`
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
	// Áp dụng quyền file (ví dụ 0600) TRƯỚC khi ghi dữ liệu nhạy cảm để tránh cửa sổ race condition.
	if mode != 0 {
		if err := sftpClient.Chmod(remotePosixPath, mode); err != nil {
			_ = dst.Close()
			return fmt.Errorf("sftp chmod %s on %s: %w", remotePosixPath, c.nodeName, err)
		}
	}
	if _, err := dst.Write(content); err != nil {
		_ = dst.Close()
		return fmt.Errorf("sftp write %s on %s: %w", remotePosixPath, c.nodeName, err)
	}
	return dst.Close()
}

// DownloadFile tải một file từ Target Node Linux về máy Deployer bằng Pure Go SFTP (quyền 0600 để bảo vệ dữ liệu nhạy cảm như backup PKI/etcd).
func (c *Client) DownloadFile(ctx context.Context, remotePosixPath, localPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sftpClient, err := sftp.NewClient(c.conn)
	if err != nil {
		return fmt.Errorf("open sftp session on %s: %w", c.nodeName, err)
	}
	defer sftpClient.Close()

	src, err := sftpClient.Open(remotePosixPath)
	if err != nil {
		return fmt.Errorf("sftp open remote %s on %s: %w", remotePosixPath, c.nodeName, err)
	}
	defer src.Close()

	if dir := filepath.Dir(localPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create local dir %q: %w", dir, err)
		}
	}
	dst, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create local file %q: %w", localPath, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(localPath)
		return fmt.Errorf("sftp download %s -> %s: %w", remotePosixPath, localPath, err)
	}
	return dst.Close()
}

// ListDirNames liệt kê danh sách tên file trong thư mục remotePosixDir qua Pure Go SFTP.
func (c *Client) ListDirNames(ctx context.Context, remotePosixDir string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sftpClient, err := sftp.NewClient(c.conn)
	if err != nil {
		return nil, fmt.Errorf("open sftp session on %s: %w", c.nodeName, err)
	}
	defer sftpClient.Close()

	infos, err := sftpClient.ReadDir(remotePosixDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		if !info.IsDir() {
			names = append(names, info.Name())
		}
	}
	return names, nil
}

// RemoveFile xóa một file từ xa trên máy chủ SFTP bằng Pure Go SFTP.
func (c *Client) RemoveFile(ctx context.Context, remotePosixPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sftpClient, err := sftp.NewClient(c.conn)
	if err != nil {
		return fmt.Errorf("open sftp session on %s: %w", c.nodeName, err)
	}
	defer sftpClient.Close()
	return sftpClient.Remove(remotePosixPath)
}

// buildHostKeyCallback xác thực SSH host key qua ~/.ssh/known_hosts theo cơ chế TOFU (Trust On First Use):
// tự động lưu host key lần đầu kết nối, và TỪ CHỐI ngay nếu host key bị thay đổi (chống MITM).
func buildHostKeyCallback(cfg api.SSHConfig) (gossh.HostKeyCallback, error) {
	if cfg.InsecureIgnoreHostKey {
		return gossh.InsecureIgnoreHostKey(), nil
	}
	khPath := cfg.KnownHostsPath
	if khPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir for known_hosts: %w", err)
		}
		khPath = filepath.Join(home, ".ssh", "known_hosts")
	} else {
		var err error
		khPath, err = expandHomePath(khPath)
		if err != nil {
			return nil, err
		}
	}

	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()

		if err := os.MkdirAll(filepath.Dir(khPath), 0o700); err != nil {
			return fmt.Errorf("prepare known_hosts dir: %w", err)
		}
		f, err := os.OpenFile(khPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
		if err != nil {
			return fmt.Errorf("open known_hosts %q: %w", khPath, err)
		}
		_ = f.Close()

		cb, err := knownhosts.New(khPath)
		if err != nil {
			return fmt.Errorf("parse known_hosts %q: %w", khPath, err)
		}

		err = cb(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
			// Host chưa từng thấy -> TOFU: in fingerprint và ghi nhận vào known_hosts
			fmt.Fprintf(os.Stderr, "[ssh-tofu] Trusting new host key for %s (%s)\n", hostname, gossh.FingerprintSHA256(key))
			line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
			af, openErr := os.OpenFile(khPath, os.O_APPEND|os.O_WRONLY, 0o600)
			if openErr != nil {
				return fmt.Errorf("append known_hosts: %w", openErr)
			}
			_, writeErr := af.WriteString(line + "\n")
			_ = af.Close()
			return writeErr
		}
		return fmt.Errorf("SSH host key verification failed for %s (possible MITM or VM was re-imaged; remove old entry from %s or pass --insecure-skip-host-key-check): %w", hostname, khPath, err)
	}, nil
}

func buildAuthMethods(cfg api.SSHConfig) ([]gossh.AuthMethod, io.Closer, error) {
	var (
		methods   []gossh.AuthMethod
		agentConn io.Closer
	)

	// 1. Ưu tiên Private Key chỉ định tường minh trước ssh-agent để tránh lỗi "Too many authentication failures"
	if cfg.PrivateKeyPath != "" {
		keyPath, err := expandHomePath(cfg.PrivateKeyPath)
		if err != nil {
			return nil, nil, err
		}
		keyBytes, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read ssh private key %q: %w", keyPath, err)
		}
		var signer gossh.Signer
		if cfg.PrivateKeyPassphrase != "" {
			signer, err = gossh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(cfg.PrivateKeyPassphrase))
		} else {
			signer, err = gossh.ParsePrivateKey(keyBytes)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("parse ssh private key %q: %w", keyPath, err)
		}
		methods = append(methods, gossh.PublicKeys(signer))
	}

	// 2. Hỗ trợ ssh-agent qua biến môi trường SSH_AUTH_SOCK (đóng fd khi Client.Close())
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.DialTimeout("unix", sock, 2*time.Second); err == nil {
			agentConn = conn
			methods = append(methods, gossh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	// 3. Password & KeyboardInteractive
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
		if agentConn != nil {
			_ = agentConn.Close()
		}
		return nil, nil, fmt.Errorf("no SSH auth method configured (provide privateKeyPath, password, or SSH_AUTH_SOCK)")
	}
	return methods, agentConn, nil
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
