<div align="center">

```text
        _          _
 __   _| | ___   _| |__   ___
 \ \ / / |/ / | | | '_ \ / _ \
  \ V /|   <| |_| | |_) |  __/
   \_/ |_|\_\\__,_|_.__/ \___|
```

**Fast, Pure-Go Vanilla Kubernetes Installer (`kubeadm`) — Built for On-premise & Air-gapped**

[![Release](https://img.shields.io/github/v/release/thaivu98/vkube?style=flat-square&color=blue)](https://github.com/thaivu98/vkube/releases)
[![Go](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/Deployer-Linux%20%7C%20macOS%20%7C%20Windows-success?style=flat-square)](#)
[![Arch](https://img.shields.io/badge/Target%20Nodes-amd64%20%7C%20arm64-orange?style=flat-square)](#)
[![License](https://img.shields.io/badge/License-Apache%202.0-lightgrey?style=flat-square)](LICENSE)

</div>

---

**Trình cài đặt Vanilla Kubernetes nhanh, đóng gói sẵn dựa trên `kubeadm` — tối ưu cho môi trường On-premise và Air-gapped.**

`vkube` đóng gói toàn bộ quy trình dựng cụm Vanilla Kubernetes chuẩn CNCF vào một file binary **Pure Go** duy nhất kết hợp với cơ chế **Offline Bundle (`.tar.zst`)**, mang lại trải nghiệm cài đặt **nhanh, gọn gần như K3s (~2–3 phút)** nhưng vẫn giữ nguyên 100% tính tương thích của `kubeadm` và Kubernetes upstream.

## Tính năng nổi bật

- **Cross-platform Deployer (100% Pure Go):** Chạy độc lập trên **Linux, macOS và Windows** (`amd64` & `arm64`), không cần cài Python, Ansible, Docker hay bất kỳ công cụ đặc thù nào của hệ điều hành.
- **Multi-arch Target Nodes:** Hỗ trợ đồng thời cả **`linux/amd64` và `linux/arm64`** (Ubuntu 22.04 / 24.04 / 26.x). Tự động phát hiện OS/Arch của server đích một lần duy nhất ở bước `preflight` (`NodeFacts`) và hỗ trợ build chéo bundle qua cờ `--arch amd64|arm64`.
- **Air-gapped First & BOM không hardcode:** Toàn bộ version, URL tải artifact và danh sách OCI images được quản lý tập trung trong file BOM [versions.yaml](pkg/versions/versions.yaml) (nhúng sẵn qua `//go:embed`) và có thể ghi đè ngay bằng cờ `--manifest <path>`.
- **HA Control Plane & Bảo vệ CP mặc định:** Tự động dựng Stacked `etcd` HA + **`kube-vip`** (static pod ARP L2, kiểm tra chống trùng IP VIP trong LAN), giữ chặt taint `NoSchedule` bảo vệ Control Plane khỏi workload ứng dụng.
- **Modular Core Addons & Private Registry:**
  - Bật/tắt linh hoạt trong `cluster.yaml` (cài 100% offline từ bundle): **Metrics Server** (HPA / `kubectl top`), **Local Path Provisioner** (Default StorageClass), **MetalLB** (L2 LoadBalancer), **Kong Ingress Controller** (DB-less API Gateway) và **`customManifests`** tùy chọn.
  - Hỗ trợ khai báo **Private Registry / Harbor nội bộ** (`spec.registry.mirrors`) tự động sinh `/etc/containerd/certs.d/<host>/hosts.toml`.

---

## Tốc độ triển khai thực tế (Lab Benchmark)

_Đo trực tiếp trên cụm lab VMware Fusion ARM64 (Ubuntu 26.10, 2 vCPU / 2 GB RAM / node, LAN 1Gbps). Các tính năng vòng đời Day-2 (`upgrade`, `add-node`, `etcd backup/restore`) nằm trong lộ trình v0.2–v0.5._

| Quy mô cụm                                                         | Chế độ           | Thời gian `vkube apply`               |
| :----------------------------------------------------------------- | :--------------- | :------------------------------------ |
| **2 nodes** (1 CP + 1 Worker, K8s `v1.32.2` + Calico)              | Offline Bundle   | **2 phút 4 giây** _(đã đo thực tế)_   |
| **3 nodes** (1 CP + 2 Workers + Kong, MetalLB, Metrics, LocalPath) | Offline Bundle   | **2 phút 29 giây** _(đã đo thực tế)_  |
| **3 nodes** (1 CP + 2 Workers + Kong, MetalLB, Metrics, LocalPath) | Online trực tiếp | **2 phút 43 giây** _(đã đo thực tế)_  |

---

## Hướng dẫn cài đặt & Sử dụng nhanh

### Bước 0: Cài đặt `vkube` CLI trên máy Deployer

> **Lưu ý:** `vkube` là **1 file binary độc lập (Pure Go)** — không cần `apt install`, không cần Python, Ansible hay Docker.

#### Cách 1: Cài nhanh 1 lệnh từ GitHub Releases _(Khuyên dùng)_

- **Linux / macOS (cài đúng phiên bản theo tag):**

  ```bash
  curl -fsSL https://raw.githubusercontent.com/thaivu98/vkube/v1.32.2-vkube.1/scripts/install.sh | sudo sh
  ```

- **Hoặc luôn cài bản mới nhất (`latest`):**

  ```bash
  VKUBE_VERSION=latest curl -fsSL https://raw.githubusercontent.com/thaivu98/vkube/main/scripts/install.sh | sudo sh
  ```

  _(Script tự phát hiện OS/CPU, tải đúng binary từ [GitHub Releases](https://github.com/thaivu98/vkube/releases), cấp quyền thực thi và cài thẳng vào `/usr/local/bin/vkube`.)_

- **Windows:** Tải trực tiếp `vkube-windows-amd64.exe` tại [GitHub Releases](https://github.com/thaivu98/vkube/releases).

#### Cách 2: Cài từ mã nguồn _(khi chưa có bản Release hoặc muốn tự build)_

```bash
git clone https://github.com/thaivu98/vkube.git && cd vkube
./scripts/init-common.sh && make build && sudo cp bin/vkube /usr/local/bin/vkube
```

---

### Yêu cầu trên các Server đích (Target Nodes)

- Hệ điều hành: **Ubuntu 22.04 / 24.04 / 26.x** (`amd64` hoặc `arm64`).
- Tài nguyên tối thiểu: **2 vCPU, 2 GB RAM, 20 GB Disk** cho Control Plane.
- Đã bật **SSH** và có user quyền `sudo` (hỗ trợ SSH Key hoặc Password).
- **Không cần kết nối Internet** trên các server đích.

---

### Quy trình dựng cụm Kubernetes

#### Chế độ 1: Online trực tiếp *(máy chủ có Internet — chỉ 2 lệnh, không cần build bundle!)*

```bash
# Bước 1: Sinh file cấu hình mẫu cluster.yaml đầy đủ hướng dẫn (thêm --ha cho cụm 3 CP)
vkube init-config -o cluster.yaml
# -> Điền IP, SSH key/password, VIP (nếu HA) và bật/tắt addons trong cluster.yaml

# Bước 2: Triển khai cụm Kubernetes ngay lập tức (~2–3 phút)
vkube apply -f cluster.yaml

# Bước 3: Kiểm tra trạng thái cụm (Nodes, All Pods, StorageClasses, LoadBalancer, kubectl top)
vkube status -f cluster.yaml
```

#### Chế độ 2: Air-gapped *(mạng kín không Internet — dùng Offline Bundle)*

```bash
# Trên máy CÓ Internet: tải sẵn binary vkube và đóng gói Offline Bundle (làm 1 lần, dùng cho nhiều cụm)
vkube bundle build --arch amd64
vkube bundle verify bundles/vkube-bundle-v1.32.2-vkube.1-linux-amd64.tar.zst

# Chép 2 file (binary vkube + file .tar.zst) vào vùng Air-gapped rồi chạy:
vkube init-config -o cluster.yaml
vkube apply -f cluster.yaml --bundle vkube-bundle-v1.32.2-vkube.1-linux-amd64.tar.zst
```

Sau khi hoàn tất, `vkube` tự động xuất file `kubeconfig-<ten-cum>.yaml` ngay tại thư mục hiện tại:

```bash
kubectl --kubeconfig kubeconfig-my-cluster.yaml get nodes,pods -A
```

---

## Tùy chỉnh phiên bản bằng `--manifest` (Không cần build lại `vkube`)

```bash
# Xuất file BOM mặc định ra ngoài
vkube bundle export-manifest -o my-versions.yaml

# Sửa bất kỳ dòng version nào trong my-versions.yaml rồi build đè ngay
vkube bundle build --arch amd64 --manifest my-versions.yaml
```

## Các lệnh CLI chính

| Lệnh                                                                     | Chức năng                                                            |
| :----------------------------------------------------------------------- | :------------------------------------------------------------------- |
| `vkube version [--manifest <file>]`                                      | Xem phiên bản `vkube` và toàn bộ bảng phiên bản thành phần trong BOM |
| `vkube init-config [--arch amd64\|arm64] [--ha] [-o file]`               | Sinh nhanh file `cluster.yaml` mẫu                                   |
| `vkube bundle build [--arch amd64\|arm64] [--manifest file] [--dry-run]` | Đóng gói Offline Bundle `.tar.zst` (100% Pure Go, không cần Docker)  |
| `vkube bundle verify <bundle.tar.zst>`                                   | Xác thực toàn bộ SHA256 checksum & metadata của Offline Bundle       |
| `vkube bundle export-manifest [-o versions.yaml]`                        | Xuất file BOM mặc định để tùy chỉnh                                  |
| `vkube apply -f <cluster.yaml> --bundle <file.tar.zst>`                  | Triển khai cụm Kubernetes + Core Addons + xuất `kubeconfig`          |
| `vkube status -f <cluster.yaml>`                                         | Xem trạng thái Nodes, Pods, StorageClasses, LoadBalancer & Metrics   |
| `vkube reset -f <cluster.yaml> --yes`                                    | Gỡ bỏ cụm (`kubeadm reset`) và dọn sạch cấu hình trên toàn bộ nodes  |

## Build đa nền tảng (Cross-compile)

```bash
make build        # Build cho OS/arch hiện tại -> bin/vkube
make release-all  # Cross-compile đủ 6 nền tảng (linux/darwin/windows x amd64/arm64) -> dist/
make test         # Chạy toàn bộ unit tests
```

## Quy tắc phiên bản (Versioning)

Tag phát hành của `vkube` bám sát phiên bản Kubernetes theo định dạng `v<k8s-version>-vkube.<rev>` (ví dụ: `v1.32.2-vkube.1`).

## Tài liệu dự án

- [Kiến trúc hệ thống (Architecture)](docs/ARCHITECTURE.md)
- [Kế hoạch phát triển &amp; Trạng thái hiện tại (Plan)](docs/PLAN.md)
- [Hướng dẫn dựng Lab kiểm thử](lab/README.md)
- [File mẫu cấu hình cụm (Cluster Template)](clusters/cluster_template.yaml)
- [Quy tắc kỹ thuật &amp; Cộng tác (AGENTS.md)](AGENTS.md)

## Giấy phép

Phát hành theo giấy phép [Apache License 2.0](LICENSE).
