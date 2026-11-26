package api

import (
	"fmt"
	"runtime"
	"time"
)

const (
	DefaultAPIVersion = "vkube.io/v1alpha1"
	DefaultKind       = "Cluster"

	ArchAuto  = "auto"
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"

	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"

	CNICalico = "calico"
	CNICilium = "cilium"

	DefaultPodSubnet     = "10.244.0.0/16"
	DefaultServiceSubnet = "10.96.0.0/12"
	DefaultSSHPort       = 22
	DefaultSSHUser       = "root"
)

// DefaultLocalArch trả về kiến trúc của máy đang chạy CLI (amd64/arm64), fallback về amd64.
func DefaultLocalArch() string {
	switch runtime.GOARCH {
	case ArchAMD64, ArchARM64:
		return runtime.GOARCH
	default:
		return ArchAMD64
	}
}

// ValidateArch kiểm tra kiến trúc đích cụ thể có được vkube hỗ trợ hay không (amd64 hoặc arm64).
func ValidateArch(arch string) error {
	switch arch {
	case ArchAMD64, ArchARM64:
		return nil
	default:
		return fmt.Errorf("unsupported target architecture %q (supported: %s, %s)", arch, ArchAMD64, ArchARM64)
	}
}

// ValidateClusterArch kiểm tra trường spec.arch trong cluster.yaml (chấp nhận auto, amd64, arm64).
func ValidateClusterArch(arch string) error {
	if arch == ArchAuto {
		return nil
	}
	return ValidateArch(arch)
}

// ClusterConfig đại diện cho cấu hình cụm khai báo trong file cluster.yaml.
type ClusterConfig struct {
	APIVersion string          `yaml:"apiVersion" json:"apiVersion"`
	Kind       string          `yaml:"kind" json:"kind"`
	Metadata   ClusterMetadata `yaml:"metadata" json:"metadata"`
	Spec       ClusterSpec     `yaml:"spec" json:"spec"`
}

// ClusterMetadata chứa thông tin định danh của cụm.
type ClusterMetadata struct {
	Name string `yaml:"name" json:"name"`
}

// ClusterSpec định nghĩa cấu hình triển khai cụm Kubernetes.
type ClusterSpec struct {
	// Arch là kiến trúc CPU của các target nodes (amd64 hoặc arm64).
	Arch string `yaml:"arch" json:"arch"`
	// ControlPlaneEndpoint là endpoint HA dạng host:port (tự sinh từ VIP:6443 nếu bỏ trống).
	ControlPlaneEndpoint string `yaml:"controlPlaneEndpoint,omitempty" json:"controlPlaneEndpoint,omitempty"`
	// VIP là địa chỉ IP ảo dùng cho kube-vip khi triển khai HA Control Plane.
	VIP string `yaml:"vip,omitempty" json:"vip,omitempty"`
	// VIPInterface là tên card mạng gắn VIP (tự động phát hiện nếu bỏ trống).
	VIPInterface string `yaml:"vipInterface,omitempty" json:"vipInterface,omitempty"`
	// Networking cấu hình dải mạng Pod, Service và CNI.
	Networking NetworkingConfig `yaml:"networking,omitempty" json:"networking,omitempty"`
	// SSH là cấu hình SSH mặc định cho toàn cụm (từng node có thể ghi đè).
	SSH SSHConfig `yaml:"ssh,omitempty" json:"ssh,omitempty"`
	// Nodes là danh sách các máy chủ thành viên trong cụm.
	Nodes []NodeConfig `yaml:"nodes" json:"nodes"`
}

// NetworkingConfig định nghĩa thông số mạng của cụm Kubernetes.
type NetworkingConfig struct {
	PodSubnet     string `yaml:"podSubnet,omitempty" json:"podSubnet,omitempty"`
	ServiceSubnet string `yaml:"serviceSubnet,omitempty" json:"serviceSubnet,omitempty"`
	CNI           string `yaml:"cni,omitempty" json:"cni,omitempty"`
}

// SSHConfig chứa thông tin xác thực và kết nối SSH (hỗ trợ cả key và password).
type SSHConfig struct {
	User           string `yaml:"user,omitempty" json:"user,omitempty"`
	Port           int    `yaml:"port,omitempty" json:"port,omitempty"`
	PrivateKeyPath string `yaml:"privateKeyPath,omitempty" json:"privateKeyPath,omitempty"`
	Password       string `yaml:"password,omitempty" json:"password,omitempty"`
	SudoPassword   string `yaml:"sudoPassword,omitempty" json:"sudoPassword,omitempty"`
}

// NodeConfig đại diện cho một Target Node Linux.
type NodeConfig struct {
	Name           string            `yaml:"name" json:"name"`
	Host           string            `yaml:"host" json:"host"`
	Role           string            `yaml:"role" json:"role"`
	User           string            `yaml:"user,omitempty" json:"user,omitempty"`
	Port           int               `yaml:"port,omitempty" json:"port,omitempty"`
	PrivateKeyPath string            `yaml:"privateKeyPath,omitempty" json:"privateKeyPath,omitempty"`
	Password       string            `yaml:"password,omitempty" json:"password,omitempty"`
	SudoPassword   string            `yaml:"sudoPassword,omitempty" json:"sudoPassword,omitempty"`
	Labels         map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// BundleManifest mô tả metadata và danh sách checksum bên trong một Offline Bundle (.tar.zst).
type BundleManifest struct {
	FormatVersion     string            `json:"formatVersion" yaml:"formatVersion"`
	ReleaseTag        string            `json:"releaseTag" yaml:"releaseTag"`
	KubernetesVersion string            `json:"kubernetesVersion" yaml:"kubernetesVersion"`
	Arch              string            `json:"arch" yaml:"arch"`
	BuiltAt           time.Time         `json:"builtAt" yaml:"builtAt"`
	Components        map[string]string `json:"components" yaml:"components"`
	Files             []BundleFile      `json:"files" yaml:"files"`
}

// BundleFile lưu đường dẫn POSIX bên trong archive và SHA256 checksum tương ứng.
type BundleFile struct {
	Path   string `json:"path" yaml:"path"`
	SHA256 string `json:"sha256" yaml:"sha256"`
	Size   int64  `json:"size" yaml:"size"`
}

// NodeFacts lưu thông tin hệ thống thu thập đúng một lần ở bước preflight cho mỗi Target Node,
// giúp các giai đoạn tiếp theo tái sử dụng ngay trong bộ nhớ mà không cần SSH kiểm tra lại.
type NodeFacts struct {
	NodeName         string `json:"nodeName"`
	Host             string `json:"host"`
	OSID             string `json:"osId"`             // ví dụ: "ubuntu"
	OSVersion        string `json:"osVersion"`        // ví dụ: "24.04"
	Arch             string `json:"arch"`             // chuẩn hóa: "amd64" hoặc "arm64"
	KernelVersion    string `json:"kernelVersion"`    // ví dụ: "6.8.0-45-generic"
	Hostname         string `json:"hostname"`         // hostname thực tế hiện tại trên OS
	DefaultInterface string `json:"defaultInterface"` // card mạng default route (dùng cho kube-vip)
	CPUCores         int    `json:"cpuCores"`
	MemoryMB         int    `json:"memoryMB"`
	HasGoldenImage   bool   `json:"hasGoldenImage"` // sẵn sàng cho v0.3 (Packer Golden Image)
}
