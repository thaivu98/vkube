package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/thaivu98/vkube/pkg/api"
)

// Pool quản lý tập kết nối SSH tới toàn bộ các node trong cụm và hỗ trợ thực thi song song.
type Pool struct {
	mu      sync.RWMutex
	runners map[string]Runner
	out     io.Writer
}

// NewPool tạo một Pool từ danh sách Runner có sẵn (hữu ích cho cả runtime lẫn unit test).
func NewPool(runners map[string]Runner, out io.Writer) *Pool {
	if out == nil {
		out = io.Discard
	}
	if runners == nil {
		runners = make(map[string]Runner)
	}
	return &Pool{runners: runners, out: out}
}

// ConnectAll mở kết nối SSH song song tới toàn bộ node khai báo trong ClusterConfig.
func ConnectAll(ctx context.Context, cfg *api.ClusterConfig, out io.Writer) (*Pool, error) {
	if out == nil {
		out = io.Discard
	}
	pool := NewPool(make(map[string]Runner, len(cfg.Spec.Nodes)), out)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []string
	)

	for _, node := range cfg.Spec.Nodes {
		wg.Add(1)
		go func(n api.NodeConfig) {
			defer wg.Done()
			sshCfg := cfg.EffectiveSSH(n)
			client, err := Dial(ctx, n.Name, n.Host, sshCfg)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Sprintf("[%s] %v", n.Name, err))
				return
			}
			pool.runners[n.Name] = client
		}(node)
	}
	wg.Wait()

	if len(errs) > 0 {
		_ = pool.Close()
		return nil, errors.New("failed to connect via SSH:\n  - " + strings.Join(errs, "\n  - "))
	}
	return pool, nil
}

// Get trả về Runner tương ứng với tên node.
func (p *Pool) Get(nodeName string) (Runner, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.runners[nodeName]
	if !ok {
		return nil, fmt.Errorf("no active SSH connection for node %q", nodeName)
	}
	return r, nil
}

// RunParallel thực thi hàm fn đồng thời trên toàn bộ danh sách nodes và gom tất cả lỗi (nếu có).
func (p *Pool) RunParallel(ctx context.Context, nodes []api.NodeConfig, fn func(ctx context.Context, node api.NodeConfig, r Runner) error) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []string
	)

	for _, node := range nodes {
		wg.Add(1)
		go func(n api.NodeConfig) {
			defer wg.Done()
			r, err := p.Get(n.Name)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("[%s] %v", n.Name, err))
				mu.Unlock()
				return
			}
			if err := fn(ctx, n, r); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("[%s] %v", n.Name, err))
				mu.Unlock()
			}
		}(node)
	}
	wg.Wait()

	if len(errs) > 0 {
		return errors.New("parallel node execution failed:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// Close đóng toàn bộ kết nối SSH trong Pool.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var firstErr error
	for name, r := range p.runners {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(p.runners, name)
	}
	return firstErr
}
