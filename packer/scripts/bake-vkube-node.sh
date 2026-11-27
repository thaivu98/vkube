#!/usr/bin/env bash
# ==============================================================================
# bake-vkube-node.sh — Script provision Golden Image (Packer) cho vkube
# ==============================================================================
# Cài sẵn OS tuning, binaries (kubeadm/kubelet/kubectl/containerd/runc/cni/crictl),
# manifests và toàn bộ OCI images từ Offline Bundle, sau đó ghi dấu /etc/vkube-golden-image
# để `vkube apply` tự động bỏ qua bước tải/nạp artifact (~1–1.5 phút lên cụm).
# ==============================================================================
set -euo pipefail

BUNDLE_PATH="${VKUBE_BUNDLE_PATH:-/tmp/vkube-bundle.tar.zst}"
RELEASE_TAG="${VKUBE_RELEASE_TAG:-v1.36.7-vkube.1}"
PAUSE_VERSION="${VKUBE_PAUSE_VERSION:-3.10.2}"

if [ ! -f "$BUNDLE_PATH" ]; then
  echo "ERROR: không tìm thấy Offline Bundle tại $BUNDLE_PATH" >&2
  exit 1
fi
if [ -d /etc/kubernetes/pki ] || [ -f /etc/kubernetes/admin.conf ]; then
  echo "ERROR: máy nguồn đã tồn tại chứng chỉ Kubernetes (/etc/kubernetes/pki) — không được bake Golden Image từ node bẩn!" >&2
  exit 1
fi

echo "==> [1/4] OS Tuning (swap, kernel modules, sysctl, SELinux)..."
swapoff -a || true
sed -ri '/\sswap\s/s/^#?/#/' /etc/fstab

if command -v setenforce >/dev/null 2>&1; then
  setenforce 0 2>/dev/null || true
fi
if [ -f /etc/selinux/config ]; then
  sed -i 's/^SELINUX=enforcing$/SELINUX=permissive/' /etc/selinux/config
fi

cat <<'EOF' > /etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF
modprobe overlay || true
modprobe br_netfilter || true

cat <<'EOF' > /etc/sysctl.d/99-kubernetes-cri.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sysctl --system >/dev/null || true

echo "==> [2/4] Giải nén Offline Bundle & cài đặt binaries..."
WORK_DIR="/var/lib/vkube/packer-staging"
rm -rf "$WORK_DIR"
mkdir -p -m 0700 "$WORK_DIR"
mkdir -p /usr/local/bin /opt/cni/bin /etc/vkube/manifests /etc/containerd /etc/containerd/certs.d /etc/systemd/system/kubelet.service.d

tar --zstd -xf "$BUNDLE_PATH" -C "$WORK_DIR"
rm -f "$BUNDLE_PATH"

install -m 0755 "$WORK_DIR/bin/"* /usr/local/bin/
if [ -d "$WORK_DIR/cni-plugins" ]; then
  install -m 0755 "$WORK_DIR/cni-plugins/"* /opt/cni/bin/
fi
if [ -d "$WORK_DIR/manifests" ]; then
  cp -f "$WORK_DIR/manifests/"* /etc/vkube/manifests/
fi

echo "==> [3/4] Cấu hình systemd (containerd + kubelet) & nạp OCI images..."
cat <<'EOF' > /etc/systemd/system/containerd.service
[Unit]
Description=containerd container runtime
Documentation=https://containerd.io
After=network.target local-fs.target

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
EOF

cat <<'EOF' > /etc/systemd/system/kubelet.service
[Unit]
Description=kubelet: The Kubernetes Node Agent
Documentation=https://kubernetes.io/docs/
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/bin/kubelet
Restart=always
StartLimitInterval=0
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

cat <<'EOF' > /etc/systemd/system/kubelet.service.d/10-kubeadm.conf
[Service]
Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
EnvironmentFile=-/etc/default/kubelet
ExecStart=
ExecStart=/usr/local/bin/kubelet $KUBELET_KUBECONFIG_ARGS $KUBELET_CONFIG_ARGS $KUBELET_KUBEADM_ARGS $KUBELET_EXTRA_ARGS
EOF

cat <<'EOF' > /etc/crictl.yaml
runtime-endpoint: unix:///run/containerd/containerd.sock
image-endpoint: unix:///run/containerd/containerd.sock
timeout: 10
EOF

SANDBOX_IMG="registry.k8s.io/pause:${PAUSE_VERSION}"
/usr/local/bin/containerd config default > /etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/g' /etc/containerd/config.toml
sed -i "s|sandbox_image = \".*\"|sandbox_image = \"${SANDBOX_IMG}\"|g" /etc/containerd/config.toml
sed -i -E "s|^([[:space:]]*sandbox = )['\"].*['\"]|\1'${SANDBOX_IMG}'|g" /etc/containerd/config.toml
sed -i 's|config_path = ""|config_path = "/etc/containerd/certs.d"|g' /etc/containerd/config.toml

systemctl daemon-reload
systemctl enable --now containerd
systemctl enable kubelet

if [ -d "$WORK_DIR/images" ]; then
  for img_tar in "$WORK_DIR/images"/*.tar; do
    [ -f "$img_tar" ] || continue
    /usr/local/bin/ctr -n k8s.io images import "$img_tar" >/dev/null
    rm -f "$img_tar"
  done
fi
echo "==> [4/4] Ghi marker /etc/vkube-golden-image & cấu hình tự tái tạo định danh (machine-id, SSH host key) khi clone..."
ARCH_RAW="$(uname -m)"
ARCH_NORM="amd64"
if [ "$ARCH_RAW" = "aarch64" ] || [ "$ARCH_RAW" = "arm64" ]; then ARCH_NORM="arm64"; fi
BOM_SHA=""
if [ -f "$WORK_DIR/bundle-manifest.json" ]; then
  BOM_SHA="$(sha256sum "$WORK_DIR/bundle-manifest.json" | awk '{print $1}')"
fi
rm -rf "$WORK_DIR"

cat <<EOF > /etc/vkube-golden-image
RELEASE_TAG=${RELEASE_TAG}
ARCH=${ARCH_NORM}
BOM_SHA256=${BOM_SHA}
BAKED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
EOF

# Đảm bảo mỗi VM clone ra tự tái tạo machine-id và SSH host keys mới ngay lần boot đầu tiên (kể cả khi không dùng cloud-init)
cat <<'EOF' > /etc/systemd/system/vkube-firstboot-identity.service
[Unit]
Description=Regenerate machine-id and SSH host keys on first boot of cloned Golden Image
Before=ssh.service sshd.service
ConditionFirstBoot=no

[Service]
Type=oneshot
ExecStart=/bin/sh -c '[ -s /etc/machine-id ] || systemd-machine-id-setup; ls /etc/ssh/ssh_host_*_key >/dev/null 2>&1 || ssh-keygen -A'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl enable vkube-firstboot-identity.service >/dev/null 2>&1 || true

truncate -s 0 /etc/machine-id || true
rm -f /var/lib/dbus/machine-id /etc/ssh/ssh_host_* || true
if command -v cloud-init >/dev/null 2>&1; then
  cloud-init clean --logs || true
fi
echo "Golden Image ${RELEASE_TAG} (${ARCH_NORM}) baked successfully!"
