package ssh

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/thaivu98/vkube/pkg/api"
)

type mockRunner struct {
	name string
	host string
	err  error
}

func (m *mockRunner) NodeName() string { return m.name }
func (m *mockRunner) Host() string     { return m.host }
func (m *mockRunner) Run(_ context.Context, _ string) (ExecResult, error) {
	return ExecResult{Stdout: "ok"}, m.err
}
func (m *mockRunner) RunSudo(_ context.Context, _ string) (ExecResult, error) {
	return ExecResult{Stdout: "ok"}, m.err
}
func (m *mockRunner) UploadFile(_ context.Context, _, _ string) error   { return m.err }
func (m *mockRunner) DownloadFile(_ context.Context, _, _ string) error { return m.err }
func (m *mockRunner) WriteFile(_ context.Context, _ []byte, _ string, _ os.FileMode) error {
	return m.err
}
func (m *mockRunner) Close() error { return nil }

func TestBuildSudoCommand(t *testing.T) {
	cmd, stdin := buildSudoCommand("root", "secret", "echo 'hi'")
	if stdin != nil {
		t.Fatalf("expected nil stdin for root user")
	}
	if !strings.HasPrefix(cmd, "bash -lc ") {
		t.Fatalf("unexpected root command: %s", cmd)
	}

	cmd, stdin = buildSudoCommand("ubuntu", "my-pass", "uname -m")
	if !strings.HasPrefix(cmd, "sudo -S -p '' bash -lc ") {
		t.Fatalf("unexpected non-root sudo command: %s", cmd)
	}
	if strings.Contains(cmd, "my-pass") {
		t.Fatalf("sudo password must never appear in command string")
	}
	passBytes, _ := io.ReadAll(stdin)
	if string(passBytes) != "my-pass\n" {
		t.Fatalf("expected password on stdin, got %q", string(passBytes))
	}
}

func TestExpandHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	got, err := expandHomePath("~/.ssh/id_ed25519")
	if err != nil {
		t.Fatalf("expandHomePath: %v", err)
	}
	want := filepath.Join(home, ".ssh", "id_ed25519")
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestPoolRunParallel(t *testing.T) {
	nodes := []api.NodeConfig{
		{Name: "cp-1", Host: "192.0.2.10", Role: api.RoleControlPlane},
		{Name: "worker-1", Host: "192.0.2.11", Role: api.RoleWorker},
	}
	pool := NewPool(map[string]Runner{
		"cp-1":     &mockRunner{name: "cp-1", host: "192.0.2.10"},
		"worker-1": &mockRunner{name: "worker-1", host: "192.0.2.11"},
	}, io.Discard)

	var count atomic.Int32
	if err := pool.RunParallel(context.Background(), nodes, func(ctx context.Context, node api.NodeConfig, r Runner) error {
		count.Add(1)
		_, err := r.RunSudo(ctx, "uname -m")
		return err
	}); err != nil {
		t.Fatalf("RunParallel failed: %v", err)
	}
	if count.Load() != 2 {
		t.Fatalf("expected 2 executions, got %d", count.Load())
	}

	// Test error aggregation
	errPool := NewPool(map[string]Runner{
		"cp-1":     &mockRunner{name: "cp-1", host: "192.0.2.10"},
		"worker-1": &mockRunner{name: "worker-1", host: "192.0.2.11", err: errors.New("boom")},
	}, io.Discard)
	err := errPool.RunParallel(context.Background(), nodes, func(ctx context.Context, node api.NodeConfig, r Runner) error {
		_, err := r.Run(ctx, "true")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "[worker-1] boom") {
		t.Fatalf("expected aggregated worker-1 error, got: %v", err)
	}
}

func TestBundleServerHandler(t *testing.T) {
	bundlePath := filepath.Join(t.TempDir(), "test.tar.zst")
	payload := []byte("vkube-offline-bundle-bytes")
	if err := os.WriteFile(bundlePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	srv, err := NewBundleServer(bundlePath)
	if err != nil {
		t.Fatalf("NewBundleServer: %v", err)
	}
	h := srv.Handler()

	// Valid tokenized path
	req := httptest.NewRequest(http.MethodGet, srv.urlPath(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != string(payload) {
		t.Fatalf("unexpected response: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Invalid path must return 404
	badReq := httptest.NewRequest(http.MethodGet, "/bundle/wrong-token/test.tar.zst", nil)
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for invalid token, got %d", badRec.Code)
	}
}
