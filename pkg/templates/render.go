package templates

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed files/*
var embeddedFS embed.FS

// KubeadmInitData chứa tham số render file kubeadm-init.yaml (v1beta4).
type KubeadmInitData struct {
	ClusterName          string
	KubernetesVersion    string
	AdvertiseAddress     string
	NodeName             string
	ControlPlaneEndpoint string
	PodSubnet            string
	ServiceSubnet        string
	CertificateKey       string
	CertSANs             []string
	ImagePullPolicy      string
	ImageRepository      string
}

// KubeadmJoinData chứa tham số render file kubeadm-join.yaml (v1beta4) để tránh lộ token/certificateKey trên command line (ps).
type KubeadmJoinData struct {
	APIServerEndpoint        string
	Token                    string
	DiscoveryTokenCACertHash string
	NodeName                 string
	ImagePullPolicy          string
	IsControlPlane           bool
	AdvertiseAddress         string
	CertificateKey           string
}

// KubeVipData chứa tham số render static pod kube-vip.yaml cho HA Control Plane.
type KubeVipData struct {
	Image           string
	VIP             string
	Interface       string
	KubeconfigPath  string
	ImagePullPolicy string
}

// ClusterTemplateData chứa tham số render file cluster.yaml mẫu đầy đủ hướng dẫn.
type ClusterTemplateData struct {
	Arch string
	HA   bool
}

func ContainerdService() []byte { return mustRead("files/containerd.service") }
func KubeletService() []byte    { return mustRead("files/kubelet.service") }
func KubeadmDropin() []byte     { return mustRead("files/10-kubeadm.conf") }
func CrictlConfig() []byte      { return mustRead("files/crictl.yaml") }

// RenderKongDBLess render manifest Kong Ingress Controller v3.x (DB-less, kèm Namespace & IngressClass)
// với phiên bản image được bơm động từ BOM (pkg/versions/versions.yaml).
func RenderKongDBLess(gatewayVersion, controllerVersion string) []byte {
	s := string(mustRead("files/kong-dbless.yaml"))
	s = strings.ReplaceAll(s, "__VKUBE_KONG_GATEWAY_VERSION__", gatewayVersion)
	s = strings.ReplaceAll(s, "__VKUBE_KONG_CONTROLLER_VERSION__", controllerVersion)
	return []byte(s)
}

// RenderClusterTemplate render file cluster.yaml mẫu đầy đủ comment tiếng Việt (Single/Standard/HA + Addons + Registry).
func RenderClusterTemplate(data ClusterTemplateData) ([]byte, error) {
	if data.Arch == "" {
		data.Arch = "auto"
	}
	return render("files/cluster_template.yaml.tmpl", data)
}

// RenderKubeadmInit render file cấu hình kubeadm init (v1beta4).
func RenderKubeadmInit(data KubeadmInitData) ([]byte, error) {
	if data.ImagePullPolicy == "" {
		data.ImagePullPolicy = "Never"
	}
	if data.ImageRepository == "" {
		data.ImageRepository = "registry.k8s.io"
	}
	return render("files/kubeadm-init.yaml.tmpl", data)
}

// RenderKubeadmJoin render file JoinConfiguration (v1beta4) với quyền 0600 (không truyền secret trên CLI).
func RenderKubeadmJoin(data KubeadmJoinData) ([]byte, error) {
	if data.ImagePullPolicy == "" {
		data.ImagePullPolicy = "Never"
	}
	return render("files/kubeadm-join.yaml.tmpl", data)
}

// RenderKubeVip render manifest static pod kube-vip cho HA Control Plane (chế độ ARP L2).
func RenderKubeVip(data KubeVipData) ([]byte, error) {
	if data.KubeconfigPath == "" {
		data.KubeconfigPath = "/etc/kubernetes/admin.conf"
	}
	if data.ImagePullPolicy == "" {
		data.ImagePullPolicy = "Never"
	}
	return render("files/kube-vip.yaml.tmpl", data)
}

func mustRead(name string) []byte {
	b, err := embeddedFS.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("embedded template %s missing: %v", name, err))
	}
	return b
}

func render(name string, data any) ([]byte, error) {
	raw, err := embeddedFS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", name, err)
	}
	t, err := template.New(name).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute template %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
