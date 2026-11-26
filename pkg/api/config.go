package api

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadClusterConfig đọc, áp dụng giá trị mặc định và validate file cluster.yaml từ đĩa.
func LoadClusterConfig(filePath string) (*ClusterConfig, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read cluster config %q: %w", filePath, err)
	}
	return ParseClusterConfig(data)
}

// ParseClusterConfig parse nội dung YAML, áp dụng giá trị mặc định và validate ClusterConfig.
func ParseClusterConfig(data []byte) (*ClusterConfig, error) {
	var cfg ClusterConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cluster config YAML: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults điền các giá trị mặc định an toàn cho các trường không bắt buộc.
func (c *ClusterConfig) ApplyDefaults() {
	if c.APIVersion == "" {
		c.APIVersion = DefaultAPIVersion
	}
	if c.Kind == "" {
		c.Kind = DefaultKind
	}
	if c.Metadata.Name == "" {
		c.Metadata.Name = "vkube-cluster"
	}
	if c.Spec.Arch == "" {
		c.Spec.Arch = ArchAuto
	}
	if c.Spec.Networking.PodSubnet == "" {
		c.Spec.Networking.PodSubnet = DefaultPodSubnet
	}
	if c.Spec.Networking.ServiceSubnet == "" {
		c.Spec.Networking.ServiceSubnet = DefaultServiceSubnet
	}
	if c.Spec.Networking.CNI == "" {
		c.Spec.Networking.CNI = CNICalico
	}
	if c.Spec.SSH.Port == 0 {
		c.Spec.SSH.Port = DefaultSSHPort
	}
	if c.Spec.SSH.User == "" {
		c.Spec.SSH.User = DefaultSSHUser
	}
	if c.Spec.ControlPlaneEndpoint == "" && c.Spec.VIP != "" {
		c.Spec.ControlPlaneEndpoint = net.JoinHostPort(c.Spec.VIP, "6443")
	}
}

// Validate kiểm tra tính hợp lệ toàn diện của ClusterConfig trước khi triển khai.
func (c *ClusterConfig) Validate() error {
	var errs []string

	if c.Kind != DefaultKind {
		errs = append(errs, fmt.Sprintf("kind must be %q, got %q", DefaultKind, c.Kind))
	}
	if strings.TrimSpace(c.Metadata.Name) == "" {
		errs = append(errs, "metadata.name is required")
	}
	if err := ValidateClusterArch(c.Spec.Arch); err != nil {
		errs = append(errs, fmt.Sprintf("spec.arch: %v", err))
	}

	if _, _, err := net.ParseCIDR(c.Spec.Networking.PodSubnet); err != nil {
		errs = append(errs, fmt.Sprintf("spec.networking.podSubnet %q is not a valid CIDR", c.Spec.Networking.PodSubnet))
	}
	if _, _, err := net.ParseCIDR(c.Spec.Networking.ServiceSubnet); err != nil {
		errs = append(errs, fmt.Sprintf("spec.networking.serviceSubnet %q is not a valid CIDR", c.Spec.Networking.ServiceSubnet))
	}
	switch c.Spec.Networking.CNI {
	case CNICalico:
		// Hỗ trợ chính thức trong v0.1
	default:
		errs = append(errs, fmt.Sprintf("spec.networking.cni %q is not supported in v0.1 (supported: %s)", c.Spec.Networking.CNI, CNICalico))
	}

	if c.Spec.VIP != "" && net.ParseIP(c.Spec.VIP) == nil {
		errs = append(errs, fmt.Sprintf("spec.vip %q is not a valid IP address", c.Spec.VIP))
	}

	if len(c.Spec.Nodes) == 0 {
		errs = append(errs, "spec.nodes must contain at least one control-plane node")
	}

	seenNames := make(map[string]struct{}, len(c.Spec.Nodes))
	seenHosts := make(map[string]struct{}, len(c.Spec.Nodes))
	cpCount := 0

	for i, n := range c.Spec.Nodes {
		prefix := fmt.Sprintf("spec.nodes[%d]", i)
		if strings.TrimSpace(n.Name) == "" {
			errs = append(errs, fmt.Sprintf("%s.name is required", prefix))
		} else {
			if _, dup := seenNames[n.Name]; dup {
				errs = append(errs, fmt.Sprintf("%s.name %q is duplicated (each Kubernetes node must have a unique name)", prefix, n.Name))
			}
			seenNames[n.Name] = struct{}{}
		}

		if strings.TrimSpace(n.Host) == "" {
			errs = append(errs, fmt.Sprintf("%s.host is required", prefix))
		} else {
			if _, dup := seenHosts[n.Host]; dup {
				errs = append(errs, fmt.Sprintf("%s.host %q is duplicated", prefix, n.Host))
			}
			seenHosts[n.Host] = struct{}{}
		}

		switch n.Role {
		case RoleControlPlane:
			cpCount++
		case RoleWorker:
		default:
			errs = append(errs, fmt.Sprintf("%s.role %q must be %q or %q", prefix, n.Role, RoleControlPlane, RoleWorker))
		}

		effSSH := c.EffectiveSSH(n)
		if effSSH.Port < 1 || effSSH.Port > 65535 {
			errs = append(errs, fmt.Sprintf("%s.port %d must be between 1 and 65535", prefix, effSSH.Port))
		}
		if effSSH.PrivateKeyPath == "" && effSSH.Password == "" {
			errs = append(errs, fmt.Sprintf("%s (%s): SSH authentication required (set privateKeyPath or password on node or spec.ssh)", prefix, n.Name))
		}

		for j, taint := range n.Taints {
			if strings.TrimSpace(taint.Key) == "" {
				errs = append(errs, fmt.Sprintf("%s.taints[%d].key is required", prefix, j))
			}
			switch taint.Effect {
			case TaintEffectNoSchedule, TaintEffectPreferNoSchedule, TaintEffectNoExecute:
			default:
				errs = append(errs, fmt.Sprintf("%s.taints[%d].effect %q must be one of %s, %s, %s",
					prefix, j, taint.Effect, TaintEffectNoSchedule, TaintEffectPreferNoSchedule, TaintEffectNoExecute))
			}
		}
	}

	if cpCount == 0 {
		errs = append(errs, "at least one node with role \"control-plane\" is required")
	}
	if cpCount > 1 && cpCount%2 == 0 {
		errs = append(errs, fmt.Sprintf("HA control-plane count must be odd for etcd quorum (got %d)", cpCount))
	}
	if cpCount > 1 && c.Spec.VIP == "" && c.Spec.ControlPlaneEndpoint == "" {
		errs = append(errs, "HA clusters with multiple control-plane nodes require spec.vip or spec.controlPlaneEndpoint")
	}
	if c.Spec.VIP != "" {
		if _, conflict := seenHosts[c.Spec.VIP]; conflict {
			errs = append(errs, fmt.Sprintf("spec.vip %q conflicts with an existing node host (VIP must be a dedicated unused IP in the same subnet)", c.Spec.VIP))
		}
	}

	if len(errs) > 0 {
		return errors.New("invalid cluster config:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// EffectiveSSH hợp nhất cấu hình SSH mặc định của cụm với cấu hình ghi đè của từng node.
func (c *ClusterConfig) EffectiveSSH(node NodeConfig) SSHConfig {
	eff := c.Spec.SSH
	if node.User != "" {
		eff.User = node.User
	}
	if node.Port != 0 {
		eff.Port = node.Port
	}
	if node.PrivateKeyPath != "" {
		eff.PrivateKeyPath = node.PrivateKeyPath
	}
	if node.Password != "" {
		eff.Password = node.Password
	}
	if node.SudoPassword != "" {
		eff.SudoPassword = node.SudoPassword
	}
	// Nếu dùng password SSH mà không khai báo sudoPassword riêng, mặc định tái sử dụng password SSH.
	if eff.SudoPassword == "" && eff.Password != "" {
		eff.SudoPassword = eff.Password
	}
	return eff
}

// ControlPlanes trả về danh sách các node có vai trò control-plane theo đúng thứ tự khai báo.
func (c *ClusterConfig) ControlPlanes() []NodeConfig {
	var out []NodeConfig
	for _, n := range c.Spec.Nodes {
		if n.Role == RoleControlPlane {
			out = append(out, n)
		}
	}
	return out
}

// Workers trả về danh sách các node có vai trò worker.
func (c *ClusterConfig) Workers() []NodeConfig {
	var out []NodeConfig
	for _, n := range c.Spec.Nodes {
		if n.Role == RoleWorker {
			out = append(out, n)
		}
	}
	return out
}
