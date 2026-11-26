---
name: vkube-dev
description: Quy ước kỹ thuật, nguyên tắc ngôn ngữ tiếng Việt, luật Go cross-platform/multi-arch và quy trình cập nhật docs/PLAN.md bắt buộc khi phát triển dự án vkube.
---

# Skill phát triển dự án `vkube`

Sử dụng skill này mỗi khi phát triển tính năng, refactor, viết test hoặc review code trong repository `vkube`.

## 1. Quy ước Ngôn ngữ Bắt buộc

- **Ngôn ngữ chính là Tiếng Việt:** Mọi tài liệu (`README.md`, `docs/*.md`), mục `## Current Status / Handover` trong `docs/PLAN.md` và phản hồi với người dùng đều phải viết bằng **tiếng Việt tự nhiên, rõ ràng**.
- **Giữ nguyên thuật ngữ kỹ thuật:** Không dịch cứng các thuật ngữ phổ biến (`Control Plane`, `Worker`, `CNI`, `Offline Bundle`, `Air-gapped`, `Container Runtime`, `kubeadm`, `Cross-platform`, `Multi-arch`...).
- **Mã nguồn Go:** Giữ tên package, struct, hàm, biến và CLI flags bằng **tiếng Anh** chuẩn Go; comment giải thích logic và CLI help có thể viết bằng tiếng Việt hoặc song ngữ ngắn gọn.

## 2. Checklist Bắt buộc Trước khi Code

1. Đọc `docs/PLAN.md` để nắm `## Current Status / Handover` và xác định task tiếp theo chưa hoàn thành.
2. Đọc `docs/ARCHITECTURE.md` để đảm bảo thiết kế bám sát Hướng B (Pure Go CLI + `kubeadm` + Offline Bundle + Calico mặc định ở v0.1).
3. Đánh dấu task đang làm là `[/]` trong `docs/PLAN.md` trước khi bắt đầu sửa code.

## 3. Quy tắc Kỹ thuật Cốt lõi

### Cross-platform Deployer (Linux / macOS / Windows)
- Toàn bộ SSH, truyền file, HTTP server nội bộ, nén/giải nén `tar.zst` và pull OCI images phải viết bằng **Pure Go**.
- Không gọi lệnh hệ điều hành ngoài (`ssh`, `scp`, `tar`, `curl`, `docker`) trên máy chạy CLI `vkube`.
- Dùng `path/filepath` cho đường dẫn cục bộ trên máy Deployer; dùng `path` cho đường dẫn trên Target Node Linux và bên trong archive.
- Không hardcode `/tmp`, `/Users` hay đường dẫn đặc thù của macOS; luôn dùng `os.TempDir()` và `os.UserHomeDir()`.

### Multi-arch Target Nodes (`amd64` & `arm64`)
- Không bao giờ giả định kiến trúc node đích trùng với `runtime.GOARCH`.
- Mọi hàm sinh URL tải artifact, pull OCI image và tạo `BundleManifest` đều phải nhận tham số `arch` tường minh (`amd64` hoặc `arm64`).
- **Tuyệt đối không hardcode version, URL tải về hay image tag trong code `.go`.** Toàn bộ thông tin này phải đọc từ file BOM (`pkg/versions/versions.yaml`, nhúng qua `//go:embed`) và luôn hỗ trợ người dùng ghi đè bằng cờ `--manifest`.

### Single Binary
- Nhúng toàn bộ script và template cấu hình (`kubeadm`, `containerd`, `kubelet`, `kube-vip`, `calico`) qua `//go:embed` trong `pkg/templates`.

## 4. Checklist Hoàn thành Task

1. Chạy unit tests (`go test ./...`).
2. Kiểm tra cross-compile (`make build` hoặc build cho `linux/darwin/windows` × `amd64/arm64`).
3. Cập nhật `docs/PLAN.md` bằng tiếng Việt:
   - Đổi checkbox task vừa xong từ `[/]` sang `[x]`.
   - Cập nhật lại `## Current Status / Handover` với nội dung vừa hoàn thành và task kế tiếp.
