# Golden Image (Packer) cho `vkube` — Dựng cụm Kubernetes trong ~1–1.5 phút

Thư mục này chứa bộ template **HashiCorp Packer** giúp đóng gói sẵn một VM Template (**Golden Image**) đã cài trước:
- Tối ưu OS (tắt swap, `overlay`, `br_netfilter`, `sysctl`, SELinux permissive).
- Toàn bộ binary Kubernetes (`kubeadm`, `kubelet`, `kubectl`, `containerd`, `runc`, `cni-plugins`, `crictl`) và manifests.
- Toàn bộ OCI container images đã nạp sẵn vào namespace `k8s.io` của `containerd`.
- File đánh dấu `/etc/vkube-golden-image` chứa `RELEASE_TAG=<tag>`.

Khi bạn clone VM từ Golden Image này rồi chạy `vkube apply -f cluster.yaml`, `vkube` tự động nhận diện `NodeFacts.HasGoldenImage` ở bước `preflight` và **bỏ qua hoàn toàn bước truyền/tải bundle & nạp OCI images** — chỉ chạy thẳng `kubeadm init/join` + CNI + Addons!

## Quy trình build Golden Image

```bash
# Bước 1: Build Offline Bundle đúng kiến trúc đích (amd64 hoặc arm64)
vkube bundle build --arch amd64

# Bước 2a: Build template cho VMware (Workstation / Fusion / vSphere)
packer init packer/vmware-ubuntu.pkr.hcl
packer build \
  -var "bundle_path=bundles/vkube-bundle-v1.36.7-vkube.1-linux-amd64.tar.zst" \
  -var "release_tag=v1.36.7-vkube.1" \
  packer/vmware-ubuntu.pkr.hcl

# Bước 2b: Hoặc build file .qcow2 cho QEMU/KVM & Proxmox VE
packer init packer/qemu-proxmox.pkr.hcl
packer build \
  -var "bundle_path=bundles/vkube-bundle-v1.36.7-vkube.1-linux-amd64.tar.zst" \
  -var "release_tag=v1.36.7-vkube.1" \
  packer/qemu-proxmox.pkr.hcl
```
