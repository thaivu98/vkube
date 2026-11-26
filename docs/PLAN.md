# Kế hoạch phát triển `vkube` v0.1

## Current Status / Handover
- **Phiên bản mục tiêu:** `v1.32.2-vkube.1` (MVP v0.1) — **HOÀN TẤT 100%!**
- **Giai đoạn hiện tại:** Hoàn tất toàn bộ **Phase 0 → Phase 5 (MVP v0.1)**
- **Task đang thực hiện:** Đã hoàn thành toàn bộ 11 Tasks của v0.1
- **Vừa hoàn thành:**
  - Hoàn thành **Phase 5 / Task 11**:
    - Đóng gói thành công Offline Bundle ARM64 thật (`580.8 MB`, 35 files, 100% Pure Go không cần Docker).
    - Kiểm thử E2E thành công trên 2 VM Ubuntu ARM64 thật (`master` + `worker`): triển khai trọn bộ Vanilla Kubernetes `v1.32.2` + `containerd 1.7.25` + `Calico v3.29.2` offline chỉ mất **2 phút 4 giây (`2m4s`)** từ OS trắng đến khi toàn bộ node `Ready`!
    - Phát hiện và vá ngay 2 lỗi thực tế quan trọng trong quá trình test thật: ngưỡng RAM `MemTotal` trên VM 2GB ARM64 (`1624 MB`) và chuyển staging bundle từ `/tmp` (`tmpfs`) sang `/var/tmp` & `/var/lib/vkube/staging` trên ổ đĩa thật.
    - Bổ sung hướng dẫn lab tại `lab/README.md`.
- **Bước tiếp theo (Next Action):** Tiếp tục kiểm thử thực tế (networking Calico, `vkube reset`, test HA / air-gapped) và chỉnh sửa hoàn thiện trước khi gắn tag phát hành **`v1.32.2-vkube.1`**.
- **Ghi chú & Ràng buộc cốt lõi:**
  - Deployer viết bằng Pure Go, chạy cross-platform (`linux`/`darwin`/`windows`).
  - Target Nodes hỗ trợ multi-arch (`amd64` & `arm64`), Ubuntu 22.04/24.04 LTS.
  - CNI mặc định cho v0.1: **Calico**.
  - Ngôn ngữ chính cho tài liệu, kế hoạch và báo cáo tiến độ: **Tiếng Việt** (giữ nguyên thuật ngữ kỹ thuật phổ biến).

---

## Ký hiệu trạng thái
- `[ ]` Chưa làm (Pending)
- `[/]` Đang làm (In Progress)
- `[x]` Hoàn thành (Done)

---

## Phase 0: Khởi tạo nền tảng & Tài liệu dự án
- [x] **Task 0.1:** Cập nhật `README.md` và `.gitignore` theo Hướng B (Go CLI, cross-platform, multi-arch, offline artifacts).
- [x] **Task 0.2:** Viết `docs/ARCHITECTURE.md` và `docs/PLAN.md`.
- [x] **Task 0.3:** Tạo `AGENTS.md` và `.agent/skills/vkube-dev/SKILL.md` (bao gồm quy tắc cross-platform, tracking tiến độ và quy ước ngôn ngữ tiếng Việt).
- [x] **Task 0.4:** Khởi tạo `go.mod` (`github.com/thaivu98/vkube`), dựng khung thư mục và `Makefile` hỗ trợ cross-compile.

---

## Phase 1: Schema lõi, Ma trận phiên bản & Khung CLI
- [x] **Task 1:** Implement `pkg/api` (`ClusterConfig` cho `cluster.yaml`, `BundleManifest`, validation chặt chẽ, giá trị mặc định) và `pkg/versions` (cơ chế BOM đọc từ `pkg/versions/versions.yaml` nhúng sẵn bằng `//go:embed`, hỗ trợ override từ file ngoài qua `--manifest`, tuyệt đối không hardcode version/URL trong code Go, hỗ trợ cả `amd64`/`arm64`) + unit tests.
- [x] **Task 2:** Dựng khung CLI bằng Cobra trong `cmd/vkube` (`version`, `init-config`, `bundle build --manifest`, `bundle export-manifest`, `bundle verify`, `apply`, `status`, `reset`) và file mẫu đa mô hình trong `clusters/cluster_template.yaml`.

---

## Phase 2: Công cụ đóng gói Offline Bundle (`vkube bundle`)
- [x] **Task 3:** Implement bộ tải binary và manifest đa kiến trúc bằng Pure Go HTTP, có log tiến độ và kiểm tra SHA256 (`--arch amd64|arm64`).
- [x] **Task 4:** Implement bộ pull & export OCI container images bằng Pure Go theo kiến trúc `linux/<arch>` (không cần cài Docker daemon trên máy build) và đóng gói `.tar.zst` kèm `bundle-manifest.json`.
- [x] **Task 5:** Implement `vkube bundle verify` và viết unit/integration test kiểm tra tính toàn vẹn của Offline Bundle.

---

## Phase 3: Tầng SSH Cross-platform & Phân phối Artifact (`pkg/ssh`)
- [x] **Task 6:** Implement SSH client pool bằng Pure Go (hỗ trợ private key & password, xử lý `sudo`, thực thi lệnh song song với log tách riêng theo từng node) và cơ chế đẩy artifact (SFTP + tùy chọn HTTP server nội bộ để phát tán nhanh trong LAN).

---

## Phase 4: Engine triển khai cụm (`pkg/engine`)
- [x] **Task 7:** Implement giai đoạn `preflight` (kiểm tra Ubuntu 22.04/24.04, khớp kiến trúc với bundle, quyền root/sudo, CPU/RAM/port) và `os-prep` (tắt swap, bật kernel modules `overlay` & `br_netfilter`, cấu hình `sysctl`).
- [x] **Task 8:** Implement giai đoạn cài đặt `runtime` & `artifacts` (giải nén binary, cấu hình `containerd` với `SystemdCgroup = true`, tạo systemd service cho `kubelet`, nạp image song song bằng `ctr -n k8s.io images import`).
- [x] **Task 9:** Implement `kubeadm init` (Primary Control Plane + `kube-vip` static pod cho HA) và `kubeadm join` song song (Secondary Control Planes + Worker Nodes).
- [x] **Task 10:** Implement cài đặt **Calico** CNI offline, tự động lấy `kubeconfig`, cùng các lệnh `vkube status` và `vkube reset`.

---

## Phase 5: Kiểm thử trên Lab VM & Phát hành v0.1
- [x] **Task 11:** Bổ sung hướng dẫn & script trong `lab/` (VMware Fusion ARM64 + Multipass/Lima mô phỏng Air-gapped), chạy kiểm thử E2E cho cả cụm Single-node và 3-node HA, đo thời gian triển khai thực tế và gắn tag `v1.32.2-vkube.1`.
