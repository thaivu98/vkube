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

	BackupTargetLocal = "local"
	BackupTargetS3    = "s3"
	BackupTargetSFTP  = "sftp"

	TaintEffectNoSchedule       = "NoSchedule"
	TaintEffectPreferNoSchedule = "PreferNoSchedule"
	TaintEffectNoExecute        = "NoExecute"

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
	// VIPBGP bật chế độ BGP cho kube-vip (khi các Control Plane nằm khác VLAN L3, thay cho L2 ARP mặc định).
	VIPBGP *BGPConfig `yaml:"vipBGP,omitempty" json:"vipBGP,omitempty"`
	// AllowSchedulingOnControlPlane cho phép gỡ taint NoSchedule trên Control Plane (mặc định false để bảo vệ CP;
	// chỉ nên bật true cho cụm Single-node không có Worker).
	AllowSchedulingOnControlPlane bool `yaml:"allowSchedulingOnControlPlane,omitempty" json:"allowSchedulingOnControlPlane,omitempty"`
	// Networking cấu hình dải mạng Pod, Service và CNI.
	Networking NetworkingConfig `yaml:"networking,omitempty" json:"networking,omitempty"`
	// Proxy cấu hình HTTP/HTTPS Proxy nội bộ cho containerd và chế độ cài đặt Online.
	Proxy ProxyConfig `yaml:"proxy,omitempty" json:"proxy,omitempty"`
	// ControlPlane tùy biến cờ nâng cao cho kube-apiserver/controller-manager/scheduler/kubelet & Encryption at Rest.
	ControlPlane ControlPlaneExtraConfig `yaml:"controlPlane,omitempty" json:"controlPlane,omitempty"`
	// Registry cấu hình Private Registry / Harbor Mirror cho containerd (Air-gapped).
	Registry RegistryConfig `yaml:"registry,omitempty" json:"registry,omitempty"`
	// Addons cho phép bật/tắt các Core Addons (Metrics Server, Local Path CSI, MetalLB, Kong Ingress) và customManifests.
	Addons AddonsConfig `yaml:"addons,omitempty" json:"addons,omitempty"`
	// Backup cấu hình đích lưu trữ cho `vkube backup` / `vkube restore` (local, s3/MinIO, sftp/NAS).
	Backup BackupConfig `yaml:"backup,omitempty" json:"backup,omitempty"`
	// SSH là cấu hình SSH mặc định cho toàn cụm (từng node có thể ghi đè).
	SSH SSHConfig `yaml:"ssh,omitempty" json:"ssh,omitempty"`
	// Nodes là danh sách các máy chủ thành viên trong cụm.
	Nodes []NodeConfig `yaml:"nodes" json:"nodes"`
}

// RegistryConfig khai báo Private Registry Mirror (Harbor/Nexus) và ImageRepository cho kubeadm.
type RegistryConfig struct {
	// ImageRepository ghi đè registry.k8s.io khi cài kubeadm (hữu ích cho mirror nội bộ tại VN/Enterprise).
	ImageRepository string           `yaml:"imageRepository,omitempty" json:"imageRepository,omitempty"`
	Mirrors         []RegistryMirror `yaml:"mirrors,omitempty" json:"mirrors,omitempty"`
}

// RegistryMirror cấu hình 1 registry mirror trong /etc/containerd/certs.d/<host>/hosts.toml.
type RegistryMirror struct {
	Host       string `yaml:"host" json:"host"`
	Endpoint   string `yaml:"endpoint" json:"endpoint"`
	Insecure   bool   `yaml:"insecure,omitempty" json:"insecure,omitempty"`
	CACertPath string `yaml:"caCertPath,omitempty" json:"caCertPath,omitempty"`
}

// AddonsConfig quản lý các Core Addons bật/tắt linh hoạt và các manifest tùy chỉnh.
type AddonsConfig struct {
	MetricsServer    MetricsServerAddon `yaml:"metricsServer,omitempty" json:"metricsServer,omitempty"`
	LocalPathStorage ToggleAddon        `yaml:"localPathStorage,omitempty" json:"localPathStorage,omitempty"`
	MetalLB          MetalLBAddon       `yaml:"metalLB,omitempty" json:"metalLB,omitempty"`
	KongIngress      ToggleAddon        `yaml:"kongIngress,omitempty" json:"kongIngress,omitempty"`
	CustomManifests  []string           `yaml:"customManifests,omitempty" json:"customManifests,omitempty"`
}

// MetricsServerAddon cấu hình Metrics Server (bật SecureTLS để dùng kubelet serverTLSBootstrap thay vì --kubelet-insecure-tls).
type MetricsServerAddon struct {
	Enabled   bool `yaml:"enabled" json:"enabled"`
	SecureTLS bool `yaml:"secureTLS,omitempty" json:"secureTLS,omitempty"`
}

// ToggleAddon đại diện cho một addon bật/tắt đơn giản.
type ToggleAddon struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// MetalLBAddon cấu hình MetalLB L2/BGP LoadBalancer cho môi trường Bare-metal / On-premise.
type MetalLBAddon struct {
	Enabled bool       `yaml:"enabled" json:"enabled"`
	IPRange string     `yaml:"ipRange,omitempty" json:"ipRange,omitempty"`
	BGP     *BGPConfig `yaml:"bgp,omitempty" json:"bgp,omitempty"`
}

// BGPConfig cấu hình định tuyến BGP cho kube-vip hoặc MetalLB (dùng khi cụm trải trên nhiều VLAN/L3).
type BGPConfig struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	LocalASN uint32 `yaml:"localASN,omitempty" json:"localASN,omitempty"`
	PeerASN  uint32 `yaml:"peerASN,omitempty" json:"peerASN,omitempty"`
	PeerIP   string `yaml:"peerIP,omitempty" json:"peerIP,omitempty"`
}

// ProxyConfig cấu hình Corporate HTTP/HTTPS Proxy cho containerd và bước tải Online.
type ProxyConfig struct {
	HTTPProxy  string `yaml:"httpProxy,omitempty" json:"httpProxy,omitempty"`
	HTTPSProxy string `yaml:"httpsProxy,omitempty" json:"httpsProxy,omitempty"`
	NoProxy    string `yaml:"noProxy,omitempty" json:"noProxy,omitempty"`
}

// ControlPlaneExtraConfig cho phép tùy biến cờ nâng cao (OIDC, Audit Log, Encryption at Rest, CIS Hardening) cho các thành phần cụm.
type ControlPlaneExtraConfig struct {
	APIServerExtraArgs         map[string]string `yaml:"apiServerExtraArgs,omitempty" json:"apiServerExtraArgs,omitempty"`
	ControllerManagerExtraArgs map[string]string `yaml:"controllerManagerExtraArgs,omitempty" json:"controllerManagerExtraArgs,omitempty"`
	SchedulerExtraArgs         map[string]string `yaml:"schedulerExtraArgs,omitempty" json:"schedulerExtraArgs,omitempty"`
	KubeletExtraArgs           map[string]string `yaml:"kubeletExtraArgs,omitempty" json:"kubeletExtraArgs,omitempty"`
	EncryptSecrets             bool              `yaml:"encryptSecrets,omitempty" json:"encryptSecrets,omitempty"`
	CISHardening               bool              `yaml:"cisHardening,omitempty" json:"cisHardening,omitempty"`
}

// BackupConfig cấu hình đích lưu trữ cho lệnh `vkube backup` và `vkube restore` (local, s3/MinIO, hoặc sftp/NAS).
type BackupConfig struct {
	Target    string             `yaml:"target,omitempty" json:"target,omitempty"`       // local | s3 | sftp
	Retention int                `yaml:"retention,omitempty" json:"retention,omitempty"` // Số bản backup giữ lại (0 = giữ tất cả)
	Schedule  string             `yaml:"schedule,omitempty" json:"schedule,omitempty"`   // Biểu thức OnCalendar systemd timer (vd: "daily", "*-*-* 02:00:00")
	Local     *LocalBackupConfig `yaml:"local,omitempty" json:"local,omitempty"`
	S3        *S3BackupConfig    `yaml:"s3,omitempty" json:"s3,omitempty"`
	SFTP      *SFTPBackupConfig  `yaml:"sftp,omitempty" json:"sftp,omitempty"`
}

// LocalBackupConfig lưu bản sao lưu về thư mục cục bộ trên máy Deployer (hoặc thư mục đã mount NFS/SMB).
type LocalBackupConfig struct {
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty"`
}

// S3BackupConfig cấu hình đẩy/tải bản sao lưu lên AWS S3 hoặc S3-compatible (MinIO, Ceph RGW, RustFS, Wasabi).
type S3BackupConfig struct {
	Endpoint           string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Bucket             string `yaml:"bucket" json:"bucket"`
	Prefix             string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	Region             string `yaml:"region,omitempty" json:"region,omitempty"`
	AccessKey          string `yaml:"accessKey,omitempty" json:"accessKey,omitempty"`
	SecretKey          string `yaml:"secretKey,omitempty" json:"secretKey,omitempty"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify,omitempty" json:"insecureSkipVerify,omitempty"`
	UsePathStyle       *bool  `yaml:"usePathStyle,omitempty" json:"usePathStyle,omitempty"`
}

// SFTPBackupConfig cấu hình đẩy/tải bản sao lưu lên máy chủ Backup / NAS từ xa qua giao thức SFTP (Pure Go).
type SFTPBackupConfig struct {
	Host                  string `yaml:"host" json:"host"`
	Port                  int    `yaml:"port,omitempty" json:"port,omitempty"`
	User                  string `yaml:"user" json:"user"`
	PrivateKeyPath        string `yaml:"privateKeyPath,omitempty" json:"privateKeyPath,omitempty"`
	PrivateKeyPassphrase  string `yaml:"privateKeyPassphrase,omitempty" json:"privateKeyPassphrase,omitempty"`
	Password              string `yaml:"password,omitempty" json:"password,omitempty"`
	RemoteDir             string `yaml:"remoteDir" json:"remoteDir"`
	InsecureIgnoreHostKey bool   `yaml:"insecureIgnoreHostKey,omitempty" json:"insecureIgnoreHostKey,omitempty"`
}

// NetworkingConfig định nghĩa thông số mạng của cụm Kubernetes.
type NetworkingConfig struct {
	PodSubnet            string `yaml:"podSubnet,omitempty" json:"podSubnet,omitempty"`
	ServiceSubnet        string `yaml:"serviceSubnet,omitempty" json:"serviceSubnet,omitempty"`
	CNI                  string `yaml:"cni,omitempty" json:"cni,omitempty"`
	KubeProxyReplacement bool   `yaml:"kubeProxyReplacement,omitempty" json:"kubeProxyReplacement,omitempty"`
}

// BastionConfig cấu hình SSH Jump Host (ProxyJump bằng Pure Go) khi Target Nodes nằm trong mạng private.
type BastionConfig struct {
	Host                  string `yaml:"host" json:"host"`
	Port                  int    `yaml:"port,omitempty" json:"port,omitempty"`
	User                  string `yaml:"user,omitempty" json:"user,omitempty"`
	PrivateKeyPath        string `yaml:"privateKeyPath,omitempty" json:"privateKeyPath,omitempty"`
	PrivateKeyPassphrase  string `yaml:"privateKeyPassphrase,omitempty" json:"privateKeyPassphrase,omitempty"`
	Password              string `yaml:"password,omitempty" json:"password,omitempty"`
	KnownHostsPath        string `yaml:"knownHostsPath,omitempty" json:"knownHostsPath,omitempty"`
	InsecureIgnoreHostKey bool   `yaml:"insecureIgnoreHostKey,omitempty" json:"insecureIgnoreHostKey,omitempty"`
}

// SSHConfig chứa thông tin xác thực và kết nối SSH (hỗ trợ SSH agent, private key có/không passphrase, password và Bastion/Jump Host).
type SSHConfig struct {
	User                  string         `yaml:"user,omitempty" json:"user,omitempty"`
	Port                  int            `yaml:"port,omitempty" json:"port,omitempty"`
	PrivateKeyPath        string         `yaml:"privateKeyPath,omitempty" json:"privateKeyPath,omitempty"`
	PrivateKeyPassphrase  string         `yaml:"privateKeyPassphrase,omitempty" json:"privateKeyPassphrase,omitempty"`
	Password              string         `yaml:"password,omitempty" json:"password,omitempty"`
	SudoPassword          string         `yaml:"sudoPassword,omitempty" json:"sudoPassword,omitempty"`
	KnownHostsPath        string         `yaml:"knownHostsPath,omitempty" json:"knownHostsPath,omitempty"`
	InsecureIgnoreHostKey bool           `yaml:"insecureIgnoreHostKey,omitempty" json:"insecureIgnoreHostKey,omitempty"`
	Bastion               *BastionConfig `yaml:"bastion,omitempty" json:"bastion,omitempty"`
}

// Taint biểu diễn một Kubernetes Node Taint áp dụng ngay khi node gia nhập cụm.
type Taint struct {
	Key    string `yaml:"key" json:"key"`
	Value  string `yaml:"value,omitempty" json:"value,omitempty"`
	Effect string `yaml:"effect" json:"effect"` // NoSchedule | PreferNoSchedule | NoExecute
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
	Taints         []Taint           `yaml:"taints,omitempty" json:"taints,omitempty"`
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
	HasZstd          bool   `json:"hasZstd"`        // kiểm tra có sẵn zstd trên node hay không
	HasGoldenImage   bool   `json:"hasGoldenImage"` // nhận diện Golden Image (Packer) để bỏ qua tải/nạp artifact
	GoldenImageTag   string `json:"goldenImageTag"` // releaseTag đã bake vào /etc/vkube-golden-image (nếu có)
	NTPSynced        bool   `json:"ntpSynced"`      // kiểm tra đồng bộ thời gian NTP/chrony/systemd-timesyncd
}
