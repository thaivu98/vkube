# AGENTS.md — Quy tắc Kỹ thuật & Cộng tác cho `vkube`

Tài liệu này quy định các nguyên tắc bắt buộc đối với mọi AI Agent và cộng tác viên khi phát triển dự án `vkube`.

## 1. Quy ước Ngôn ngữ (Bắt buộc)

- **Ngôn ngữ chính của dự án là Tiếng Việt:** Toàn bộ tài liệu (`README.md`, `docs/*.md`, `AGENTS.md`, skills), kế hoạch và báo cáo tiến độ trong `docs/PLAN.md`, cũng như mọi trao đổi/phản hồi với chủ dự án phải sử dụng **tiếng Việt tự nhiên, rõ ràng, dễ đọc**.
- **Quy tắc thuật ngữ kỹ thuật:** Giữ nguyên các thuật ngữ chuyên ngành phổ biến bằng tiếng Anh (ví dụ: `Control Plane`, `Worker`, `CNI`, `Offline Bundle`, `Air-gapped`, `Container Runtime`, `kubeadm`, `Cross-platform`, `Multi-arch`, `Static Pod`, `Preflight`, `Checksum`...) — tuyệt đối không dịch cứng sang tiếng Việt.
- **Quy tắc cho mã nguồn Go:**
  - Tên package, struct, hàm, biến, hằng số và các cờ CLI (flags) viết bằng **tiếng Anh** theo chuẩn idiomatic Go.
  - Comment giải thích logic phức tạp và phần mô tả trợ giúp (CLI help text) có thể viết bằng **tiếng Việt** (hoặc song ngữ ngắn gọn).

## 2. Quy trình Làm việc Bắt buộc (Mỗi phiên làm việc)

1. **Đọc trước khi code:** Trước khi viết hoặc sửa bất kỳ dòng code nào, BẮT BUỘC đọc:
   - `docs/PLAN.md` (đặc biệt là mục `## Current Status / Handover`)
   - `docs/ARCHITECTURE.md`
2. **Cập nhật tiến độ liên tục:**
   - Khi bắt đầu một task, đánh dấu trạng thái `[/]` trong `docs/PLAN.md`.
   - Ngay khi hoàn thành task, đánh dấu `[x]` và cập nhật lại mục `## Current Status / Handover` (Giai đoạn hiện tại, Vừa hoàn thành, Bước tiếp theo, và các ghi chú/quyết định quan trọng nếu có) bằng **tiếng Việt**.
3. **Giữ phạm vi gọn gàng:** Hoàn thành trọn vẹn từng task kèm unit test và kiểm tra build trước khi chuyển sang task tiếp theo.

## 3. Quy tắc Cross-platform & Multi-arch (Không thỏa hiệp)

`vkube` chạy trên máy Deployer **Linux, macOS và Windows** và triển khai xuống Target Nodes **Linux `amd64` và `arm64`**.

- **Chỉ dùng Pure Go cho logic trên máy Deployer:**
  - Tuyệt đối không gọi ra các công cụ CLI của hệ điều hành host (`ssh`, `scp`, `rsync`, `tar`, `zstd`, `curl`, `docker`) từ `vkube`.
  - Sử dụng các thư viện Pure Go (ví dụ: `golang.org/x/crypto/ssh`, `archive/tar`, `github.com/klauspost/compress/zstd`, `net/http`, `github.com/google/go-containerregistry`).
- **Xử lý đường dẫn (Path Handling):**
  - Dùng `path/filepath` cho các đường dẫn **cục bộ** trên máy Deployer (đảm bảo Windows dùng `\` không bị lỗi).
  - Dùng `path` (chuẩn POSIX `/`) cho các đường dẫn trên **Target Node Linux** và đường dẫn file bên trong file tar archive.
- **Không hardcode OS/Arch:**
  - Không bao giờ hardcode `amd64`, `arm64`, `darwin`, `/Users/...`, hoặc đường dẫn Homebrew trong code sản phẩm hay unit test.
  - Luôn lấy kiến trúc đích từ cờ CLI (`--arch`) hoặc từ `ClusterConfig` / `BundleManifest`.
  - Dùng `os.UserHomeDir()` và `os.TempDir()` thay vì hardcode đường dẫn thư mục home/tmp trên máy Deployer.

## 4. Tiêu chuẩn Code & Kiểm chứng

- **Go Style:** Tuân thủ `gofmt`, bọc lỗi rõ ràng (`fmt.Errorf("...: %w", err)`), truyền `context.Context` cho toàn bộ thao tác mạng và SSH để hỗ trợ hủy/timeout.
- **Templates:** Nhúng các template cấu hình node bằng `//go:embed` trong `pkg/templates` để `vkube` luôn là một file binary tự chứa duy nhất.
- **Kiểm chứng trước khi đánh dấu xong Task:**
  - Chạy `go test ./...` và đảm bảo toàn bộ test đều pass.
  - Kiểm tra cross-compile thành công (`GOOS=linux GOARCH=amd64`, `GOOS=linux GOARCH=arm64`, `GOOS=darwin GOARCH=arm64`, `GOOS=windows GOARCH=amd64`).
- **Versioning & BOM (Bill of Materials):**
  - Tuân thủ định dạng `v<k8s-version>-vkube.<rev>` (ví dụ: `v1.32.2-vkube.1`).
  - **Tuyệt đối không hardcode version, URL tải artifact hay tag OCI image trong mã nguồn Go (`.go`).** Toàn bộ version, URL template và danh sách image phải được khai báo trong file BOM mặc định (`pkg/versions/versions.yaml`, nhúng bằng `//go:embed`) và hỗ trợ ghi đè linh hoạt qua cờ `--manifest <path>`.
