package templates

import (
	"strings"
	"testing"
)

func TestStaticTemplatesEmbedded(t *testing.T) {
	if !strings.Contains(string(ContainerdService()), "/usr/local/bin/containerd") {
		t.Fatal("containerd.service missing ExecStart")
	}
	if !strings.Contains(string(KubeletService()), "/usr/local/bin/kubelet") {
		t.Fatal("kubelet.service missing ExecStart")
	}
	if !strings.Contains(string(KubeadmDropin()), "KUBELET_KUBECONFIG_ARGS") {
		t.Fatal("10-kubeadm.conf missing kubelet args")
	}
	if !strings.Contains(string(CrictlConfig()), "containerd.sock") {
		t.Fatal("crictl.yaml missing containerd socket")
	}
}

func TestRenderKubeadmInit(t *testing.T) {
	out, err := RenderKubeadmInit(KubeadmInitData{
		ClusterName:          "prod-ha",
		KubernetesVersion:    "v1.32.2",
		AdvertiseAddress:     "192.0.2.11",
		NodeName:             "cp-1",
		ControlPlaneEndpoint: "192.0.2.100:6443",
		PodSubnet:            "10.244.0.0/16",
		ServiceSubnet:        "10.96.0.0/12",
		CertificateKey:       "abcdef123456",
		CertSANs:             []string{"192.0.2.100", "192.0.2.11"},
	})
	if err != nil {
		t.Fatalf("RenderKubeadmInit failed: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kubeadm.k8s.io/v1beta4",
		"imagePullPolicy: Never",
		"controlPlaneEndpoint: \"192.0.2.100:6443\"",
		"cgroupDriver: systemd",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in rendered kubeadm config:\n%s", want, s)
		}
	}
}

func TestRenderKubeVip(t *testing.T) {
	out, err := RenderKubeVip(KubeVipData{
		Image:          "ghcr.io/kube-vip/kube-vip:v0.8.9",
		VIP:            "192.0.2.100",
		Interface:      "ens160",
		KubeconfigPath: "/etc/kubernetes/super-admin.conf",
	})
	if err != nil {
		t.Fatalf("RenderKubeVip failed: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"imagePullPolicy: Never",
		"value: \"192.0.2.100\"",
		"value: \"ens160\"",
		"path: /etc/kubernetes/super-admin.conf",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in rendered kube-vip manifest:\n%s", want, s)
		}
	}
}

func TestRenderKubeadmJoin(t *testing.T) {
	out, err := RenderKubeadmJoin(KubeadmJoinData{
		APIServerEndpoint:        "192.0.2.100:6443",
		Token:                    "abcdef.0123456789abcdef",
		DiscoveryTokenCACertHash: "sha256:deadbeef",
		NodeName:                 "cp-2",
		ImagePullPolicy:          "IfNotPresent",
		IsControlPlane:           true,
		AdvertiseAddress:         "192.0.2.12",
		CertificateKey:           "certkey123",
	})
	if err != nil {
		t.Fatalf("RenderKubeadmJoin failed: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: JoinConfiguration",
		`apiServerEndpoint: "192.0.2.100:6443"`,
		`token: "abcdef.0123456789abcdef"`,
		`certificateKey: "certkey123"`,
		`advertiseAddress: "192.0.2.12"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in rendered JoinConfiguration:\n%s", want, s)
		}
	}
}

func TestRenderKongDBLess(t *testing.T) {
	s := string(RenderKongDBLess("3.9", "3.5"))
	for _, want := range []string{
		"kind: Namespace",
		"kind: IngressClass",
		"image: docker.io/library/kong:3.9",
		"image: docker.io/kong/kubernetes-ingress-controller:3.5",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in rendered Kong v3.x manifest", want)
		}
	}
	if strings.Contains(s, "__VKUBE_KONG_") {
		t.Errorf("rendered Kong manifest still contains unreplaced placeholder")
	}
}

func TestRenderCilium(t *testing.T) {
	s := string(RenderCilium(CiliumData{
		Version:              "v1.20.5",
		PodSubnet:            "10.244.0.0/16",
		KubeProxyReplacement: true,
		K8sServiceHost:       "192.0.2.250",
		K8sServicePort:       "6443",
	}))
	for _, want := range []string{
		`quay.io/cilium/cilium:v1.20.5`,
		`quay.io/cilium/operator-generic:v1.20.5`,
		`cluster-pool-ipv4-cidr: "10.244.0.0/16"`,
		`kube-proxy-replacement: "true"`,
		`value: "192.0.2.250"`,
		`value: "6443"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in rendered Cilium manifest", want)
		}
	}
	if strings.Contains(s, "__VKUBE_") {
		t.Errorf("rendered Cilium manifest still contains unreplaced placeholder")
	}
}
