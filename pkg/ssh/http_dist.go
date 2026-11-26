package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

// BundleServer là HTTP server nội bộ viết bằng Pure Go dùng để phát tán Offline Bundle
// song song với tốc độ tối đa trong mạng LAN tới nhiều Target Nodes cùng lúc.
type BundleServer struct {
	filePath string
	token    string
	fileName string
	server   *http.Server
	listener net.Listener
}

// NewBundleServer khởi tạo một BundleServer phục vụ duy nhất file bundlePath với đường dẫn bí mật ngẫu nhiên.
func NewBundleServer(bundlePath string) (*BundleServer, error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generate bundle server token: %w", err)
	}
	return &BundleServer{
		filePath: bundlePath,
		token:    hex.EncodeToString(tokenBytes),
		fileName: filepath.Base(bundlePath),
	}, nil
}

// Handler trả về http.Handler chỉ phục vụ đúng đường dẫn có token hợp lệ.
func (s *BundleServer) Handler() http.Handler {
	expectedPath := s.urlPath()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != expectedPath {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, s.filePath)
	})
}

func (s *BundleServer) urlPath() string {
	return fmt.Sprintf("/bundle/%s/%s", s.token, s.fileName)
}

// Start lắng nghe trên địa chỉ bindAddr (ví dụ ":0" để hệ điều hành cấp port trống tự động).
func (s *BundleServer) Start(bindAddr string) error {
	if bindAddr == "" {
		bindAddr = ":0"
	}
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("listen bundle http server on %s: %w", bindAddr, err)
	}
	s.listener = ln
	s.server = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = s.server.Serve(ln)
	}()
	return nil
}

// URLForHost trả về URL đầy đủ mà các Target Node trong LAN có thể tải Offline Bundle.
func (s *BundleServer) URLForHost(hostIP string) (string, error) {
	if s.listener == nil {
		return "", fmt.Errorf("bundle server is not started")
	}
	_, port, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s%s", net.JoinHostPort(hostIP, port), s.urlPath()), nil
}

// Shutdown dừng HTTP server một cách an toàn.
func (s *BundleServer) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// OutboundIP tự động xác định địa chỉ IP mạng LAN trên máy Deployer có tuyến định tuyến tới targetHost.
func OutboundIP(targetHost string) (string, error) {
	conn, err := net.DialTimeout("udp", net.JoinHostPort(targetHost, "22"), 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("determine outbound IP toward %s: %w", targetHost, err)
	}
	defer conn.Close()
	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", fmt.Errorf("unexpected local addr type %T", conn.LocalAddr())
	}
	return localAddr.IP.String(), nil
}
