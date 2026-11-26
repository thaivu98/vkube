# vkube

**Trình cài đặt Vanilla Kubernetes nhanh, đóng gói sẵn dựa trên `kubeadm` — tối ưu cho môi trường On-premise và Air-gapped.**

`vkube` đóng gói quy trình dựng cụm Vanilla Kubernetes chuẩn CNCF vào một file binary Go duy nhất kết hợp với cơ chế **Offline Bundle**, hướng tới trải nghiệm triển khai **nhanh, gọn gần như K3s** nhưng vẫn giữ nguyên 100% tính tương thích của `kubeadm` và Kubernetes upstream.

## Tính năng nổi bật

- **Cross-platform Deployer (Pure Go):** Chạy độc lập trên **Linux, macOS và Windows**, không yêu cầu cài đặt thêm Python, Ansible hay bất kỳ công cụ đặc thù nào của hệ điều hành.
- **Multi-arch Target Nodes:** Hỗ trợ đồng thời cả **`amd64` và `arm64`** trên các node Linux (Ubuntu 22.04 / 24.04 LTS cho bản v0.1). Có thể build Offline Bundle cho bất kỳ kiến trúc đích nào từ bất kỳ máy nào thông qua cờ `--arch amd64|arm64`.
- **Ưu tiên Air-gapped (Air-gapped First):** Tách riêng hoàn toàn bước đóng gói artifact (`vkube bundle build`) và bước triển khai cụm (`vkube apply`). Toàn bộ binary, manifest và OCI container images được gom vào một file `.tar.zst` duy nhất kèm mã kiểm tra SHA256.
- **Tốc độ triển khai cao:** Thực thi SSH song song trên toàn bộ node, phân phối bundle qua luồng SFTP/HTTP cục bộ và nạp image song song bằng `ctr`, loại bỏ hoàn toàn độ trễ của các tác vụ kiểm tra đa hệ điều hành.
- **Opinionated Stack (v0.1):**
  - Kubernetes `v1.32.x` (`kubeadm`, `kubelet`, `kubectl`)
  - Container Runtime: `containerd` + `runc` + `cni-plugins` + `crictl`
  - HA Control Plane: `kube-vip` (static pod)
  - CNI: **Calico** mặc định ở v0.1 (sẽ bổ sung Cilium ở v0.2)

## Quy tắc phiên bản (Versioning)

Tag phát hành của `vkube` bám sát phiên bản Kubernetes mục tiêu theo định dạng:

```text
v<k8s-version>-vkube.<rev>
```

Ví dụ: `v1.32.2-vkube.1`

Mỗi tag khóa cứng một bộ phiên bản đã kiểm thử tương thích gồm Kubernetes, `containerd`, `runc`, `cni-plugins`, `crictl`, `Calico` và `kube-vip`.

## Quy trình sử dụng dự kiến (v0.1)

```bash
# 1. Build Offline Bundle trên máy có Internet (chọn kiến trúc đích tùy ý)
vkube bundle build --arch amd64 --output bundles/vkube-v1.32.2-linux-amd64.tar.zst
vkube bundle build --arch arm64 --output bundles/vkube-v1.32.2-linux-arm64.tar.zst

# 2. Chuyển bundle vào môi trường Air-gapped và dựng cụm
vkube apply -f examples/cluster-ha.yaml --bundle bundles/vkube-v1.32.2-linux-amd64.tar.zst
```

## Build từ mã nguồn & Cài đặt gói tiên quyết

> **Lưu ý:** Người dùng cuối tải bản binary `vkube` phát hành sẵn **không cần cài đặt bất kỳ công cụ tiên quyết nào** (100% Pure Go single binary).

Nếu bạn muốn build từ mã nguồn hoặc đóng góp code, cần có sẵn:
- **Go** `>= 1.23`
- **Git** & **Make**

Chạy script tự động kiểm tra và cài đặt toàn bộ công cụ tiên quyết trên macOS / Ubuntu Linux:

```bash
chmod +x scripts/init-common.sh
./scripts/init-common.sh
```

Sau đó build và test:

```bash
# Build cho hệ điều hành và kiến trúc hiện tại
make build

# Cross-compile cho Linux, macOS và Windows (amd64 & arm64)
make release-all

# Chạy unit test
make test
```

## Tài liệu dự án

- [Kiến trúc hệ thống (Architecture)](docs/ARCHITECTURE.md)
- [Kế hoạch phát triển v0.1 & Trạng thái hiện tại (Plan)](docs/PLAN.md)
- [Quy tắc kỹ thuật & Cộng tác (AGENTS.md)](AGENTS.md)

## Giấy phép

Phát hành theo giấy phép [Apache License 2.0](LICENSE).
