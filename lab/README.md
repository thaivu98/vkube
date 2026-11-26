# Hướng dẫn dựng Lab kiểm thử `vkube` (VMware Fusion / Air-gapped)

Tài liệu này hướng dẫn cách dựng và kiểm thử `vkube` trên cụm VM local Ubuntu 22.04 / 24.04 LTS (`amd64` hoặc `arm64`), bao gồm cả kịch bản mô phỏng môi trường **Air-gapped** hoàn toàn.

## 1. Yêu cầu cấu hình VM tối thiểu

- **OS:** Ubuntu 22.04 LTS hoặc 24.04 LTS (Server)
- **Control Plane:** tối thiểu `2 vCPU`, `2 GB RAM`, `20 GB Disk`
- **Worker:** tối thiểu `2 vCPU`, `2 GB RAM`, `20 GB Disk`
- **SSH:** Bật sẵn `openssh-server`, user có quyền `sudo` (hỗ trợ SSH Key hoặc Password)

## 2. Quy trình kiểm thử End-to-End (E2E)

### Bước 1: Build binary `vkube` và đóng gói Offline Bundle

```bash
make build

# Tự động nhận diện kiến trúc máy hiện tại (hoặc thêm --arch amd64 / --arch arm64)
./bin/vkube bundle build

# Kiểm tra tính toàn vẹn SHA256 của gói vừa đóng gói
./bin/vkube bundle verify bundles/vkube-bundle-v1.32.2-vkube.1-linux-arm64.tar.zst
```

### Bước 2: Khai báo cụm lab từ file mẫu

```bash
cp clusters/cluster_template.yaml clusters/lab.local.yaml
# Sửa IP, user/password hoặc SSH key trong clusters/lab.local.yaml
./bin/vkube apply -f clusters/lab.local.yaml --dry-run
```

_(Các file đuôi `_.local.yaml`đã được`.gitignore` chặn sẵn để không bao giờ bị push nhầm lên Git.)\*

### Bước 3: Triển khai cụm & Kiểm tra trạng thái

```bash
./bin/vkube apply -f clusters/lab.local.yaml \
  --bundle bundles/vkube-bundle-v1.32.2-vkube.1-linux-arm64.tar.zst

# Kiểm tra trạng thái Nodes & kube-system Pods
./bin/vkube status -f clusters/lab.local.yaml
```

### Bước 4: Dọn dẹp / Cài lại từ đầu (khi cần test lặp)

```bash
./bin/vkube reset -f clusters/lab.local.yaml --yes
```

## 3. Mô phỏng môi trường Air-gapped 100%

Để chứng minh `vkube` không hề cần Internet trên các Target Nodes:

- Trong **VMware Fusion / vSphere / Proxmox**, chuyển card mạng của các VM sang mạng **Host-Only** (chỉ giao tiếp nội bộ giữa máy Deployer và các VM, không có route ra Internet).
- Chạy `./bin/vkube apply -f clusters/lab.local.yaml --bundle ...` — toàn bộ binaries, `containerd`, `kubeadm`, `kube-vip`, `Calico` và OCI images đều được cài trực tiếp từ file `.tar.zst`.
