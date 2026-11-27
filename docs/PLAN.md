# Kế hoạch phát triển `vkube` (v0.1 → v1.0)

## Current Status / Handover
- **Phiên bản đã phát hành:** `v1.32.2-vkube.1` (MVP v0.1); **BOM mặc định hiện tại trên `main`:** `v1.36.7-vkube.1` (v0.2)
- **Giai đoạn hiện tại:** Hoàn tất toàn bộ **Phase 6 (v0.2 — Tasks 12 → 16)** — Chờ kiểm tra chéo bản v0.2, sau đó chuyển sang **Phase 7 (v0.3 — Enterprise Readiness & Day-2 Automation)**
- **Task đang thực hiện:** Đã hoàn thành toàn bộ Phase 6 (v0.2)
- **Vừa hoàn thành:**
  - **Task 16 (LAN HTTP Fan-out & Rocky/AlmaLinux/RHEL 8–9):** Nối `ssh.BundleServer` vào `pkg/engine/runtime.go` (cờ `--fast-sync` hoặc tự bật với cụm >= 3 nodes, tự fallback SFTP nếu bị firewall chặn); mở rộng `preflight` & `os-prep` hỗ trợ **Rocky Linux / AlmaLinux / RHEL 8–9** (SELinux permissive + mở đúng port K8s/CNI trên `firewalld` nếu đang chạy).
  - **Task 15 (Cilium eBPF CNI):** Bổ sung hỗ trợ `spec.networking.cni: cilium` và `spec.networking.kubeProxyReplacement: true` (`--skip-phases=addon/kube-proxy`), nhúng manifest `cilium/cilium v1.20.5` vào `pkg/templates/files/cilium.yaml` bơm động version/PodSubnet/API endpoint, đóng gói sẵn image Cilium trong Offline Bundle và bổ sung dọn dẹp Cilium/BPF trong `vkube reset`.
  - **Task 14 (`vkube backup` & `vkube restore` `etcd` — Multi-target):** Chụp snapshot `etcd` kèm verify (`etcdutl`/`etcdctl snapshot status`), đóng gói cùng `/etc/kubernetes/pki/` & `admin.conf`; hỗ trợ chọn đích lưu trữ qua `spec.backup` hoặc cờ CLI: **`local`**, **`s3`** (AWS S3 / MinIO / Ceph RGW viết bằng **Pure Go AWS SigV4**, không kéo AWS SDK), và **`sftp`** (NAS / Backup Server qua Pure Go SFTP); hỗ trợ khôi phục cụm 1-node và 3-node HA chống split-brain từ cả file cục bộ, `s3://...` lẫn `sftp://...`.
  - **Task 13 (`vkube add-node` & `vkube remove-node`):** Mở rộng cụm đang chạy (tự nhận diện node mới hoặc chỉ định `--node`, join CP tuần tự qua `upload-certs` + `kube-vip` và Worker song song); gỡ node an toàn (`drain` → gỡ `etcd` member nếu là CP → `delete node` → reset node, bảo vệ không cho gỡ CP cuối cùng).
  - **Task 12 (BOM v1.36.7 + Containerd v2 + Kong v3.x):**
    - Nâng BOM mặc định (`pkg/versions/versions.yaml`) lên nhánh Kubernetes đang được hỗ trợ chính thức **`v1.36.7`** (`etcd 3.6.8-0`, `coredns v1.14.2`, `pause 3.10.2`), **`containerd 2.4.2`**, `runc v1.5.3`, `cni-plugins v1.10.0`, `crictl v1.36.0`, **`Calico v3.33.1`** (chuyển sang `quay.io/calico/{calico,node,third-party-cni-plugins}`), **`kube-vip v1.3.2`**, `metrics-server v0.9.1`, `local-path v0.0.37`, `metallb v0.16.2` — toàn bộ manifest đều ghim SHA256 64-hex.
    - Nâng cấp **Kong Ingress Controller** lên dòng **v3.x** (`kong_controller: 3.5` / `kong_gateway: 3.9`), nhúng manifest DB-less render từ Helm chart chính thức `kong/kong v3.4.1` vào `pkg/templates/files/kong-dbless.yaml` kèm tự tạo `Namespace`/`IngressClass` và bơm version động từ BOM (không phụ thuộc URL cũ đã deprecated).
    - Nâng cấp `pkg/engine/runtime.go` tương thích cấu hình `/etc/containerd/config.toml` của cả **containerd 1.x (`sandbox_image`)** lẫn **containerd 2.x (`sandbox`)**, và mở rộng patch `busybox` của `local-path v0.0.37` cho `docker.io/library/busybox`.
  - Phát hành chính thức tag **`v1.32.2-vkube.1`** kèm GitHub Actions tự động build 6 binary đa nền tảng + `SHA256SUMS` và script cài 1 dòng `scripts/install.sh` (tự verify SHA256).
  - Kiểm thử E2E thành công trên cụm 3 VM Ubuntu ARM64 thật:
    - **Offline Bundle (1 CP + 2 Worker + đủ 4 Core Addons):** **2 phút 29 giây (`2m29s`)**.
    - **Online trực tiếp (1 CP + 2 Worker + đủ 4 Core Addons + verify SHA256 + SSH TOFU):** **2 phút 43 giây (`2m43s`)**.
    - **3-Control-Plane HA + `kube-vip` VIP (Online, 3× `etcd` Raft + `JoinConfiguration v1beta4` `controlPlane.certificateKey`):** **100% thành công** (3/3 Control Plane `Ready`, `kubectl` truy cập trực tiếp qua VIP `:6443`; đã xử lý `--ignore-preflight-errors=Mem` cho Secondary CP RAM 2GB và dọn địa chỉ `VIP/32` tồn dư khi chạy `vkube reset`).
  - Hoàn thiện đợt **Hardening bảo mật & độ tin cậy** từ cross-review (SSH TOFU `known_hosts` + `ssh-agent` + passphrase, Online SHA256 verify trên node, bỏ `/tmp` dùng `~/.vkube-tmp` `0600`, `vkube reset` chỉ xóa chain `KUBE-*/cali-*`, DNS-1123 validation, `VKUBE_SSH_PASSWORD` env, `--node-ip` cho multi-NIC, timing từng giai đoạn và tự in `status` sau `apply`).
- **Bước tiếp theo (Next Action):** Chọn bắt đầu **v0.2** từ các task trong Phase 6 bên dưới (gợi ý ưu tiên: **Task 12** nâng BOM K8s mới nhất + Kong 3.x, hoặc **Task 13/14** `vkube add-node` & `vkube backup`/`restore` `etcd`).
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

---

## Phase 6 (v0.2): Vận hành Day-2, Nâng cấp BOM & Mở rộng Hạ tầng
- [x] **Task 12 — Nâng cấp BOM & Addons mới nhất:** Kiểm tra support matrix mới nhất trên `kubernetes.io`, nâng BOM `versions.yaml` lên nhánh Kubernetes đang được hỗ trợ chính thức (`v1.35.x+`), cập nhật `containerd`, `Calico`, `kube-vip` tương thích và nâng **Kong Ingress Controller** lên dòng **v3.x**.
- [x] **Task 13 — Scale cụm (`vkube add-node` & `vkube remove-node`):** Hỗ trợ thêm mới Worker/Control Plane vào cụm đang chạy và drain + gỡ node an toàn (kèm dọn `etcd` member nếu gỡ CP).
- [x] **Task 14 — Backup & Restore `etcd` (`vkube backup` & `vkube restore`):** Tự động snapshot `etcd` (verify bằng `etcdctl snapshot status`) kèm sao lưu `/etc/kubernetes/pki/` về máy Deployer, và hỗ trợ khôi phục cụm 1-node / 3-node HA không bị split-brain.
- [x] **Task 15 — Bổ sung CNI Cilium (eBPF):** Hỗ trợ chọn `spec.networking.cni: cilium` song song với `calico` (kèm tùy chọn chạy không cần `kube-proxy` và đóng gói offline trong bundle).
- [x] **Task 16 — LAN HTTP Fan-out & Hỗ trợ Rocky/AlmaLinux/RHEL:** Nối `BundleServer` (`pkg/ssh/http_dist.go`, cờ `--fast-sync` hoặc tự bật với cụm >= 3 nodes) để phân phối bundle tốc độ cao trong LAN; mở rộng `preflight`/`os-prep` hỗ trợ thêm **Rocky Linux / AlmaLinux / RHEL 8–9** bên cạnh Ubuntu.

---

## Phase 7 (v0.3): Enterprise Readiness & Vận hành Day-2 Nâng cao
- [ ] **Task 17 — SSH Bastion (Jump Host) & Corporate HTTP Proxy:** Hỗ trợ kết nối SSH xuyên qua Jump Host (`spec.ssh.bastion`, viết bằng Pure Go `golang.org/x/crypto/ssh`) cho cụm trong private subnet; hỗ trợ `spec.proxy` (`httpProxy`, `httpsProxy`, `noProxy`) tự sinh systemd drop-in cho `containerd` và áp dụng cho bước tải Online.
- [ ] **Task 18 — Quản lý Chứng chỉ (`vkube certs check/renew`) & Backup Retention/Schedule:** Thêm lệnh kiểm tra hạn cert (kèm cảnh báo trong `vkube status`) và gia hạn cert an toàn trên toàn bộ Control Plane (`kubeadm certs renew all` + restart static pods); bổ sung `spec.backup.retention` (giữ N bản mới nhất, tự dọn bản cũ trên Local/S3/SFTP) và tùy chọn lập lịch backup `etcd` định kỳ.
- [ ] **Task 19 — Quản lý Addons & Reconcile cấu hình cụm đang chạy (`vkube addons apply`):** Cho phép bật/cập nhật thêm Core Addons, `customManifests`, Labels/Taints và Private Registry mirrors trên cụm đang vận hành mà không cần dựng lại từ đầu.
- [ ] **Task 20 — Tùy biến nâng cao `kube-apiserver`/`kubelet` & BGP Mode:** Hỗ trợ truyền `extraArgs` cho `kube-apiserver` / `kubelet` (tích hợp OIDC/Keycloak, Audit Log, Encryption at Rest cho Secrets trong `etcd`) và bổ sung chế độ **BGP** cho `kube-vip` / MetalLB (hỗ trợ hạ tầng L3 khác VLAN).

---

## Phase 8 (v0.4): Golden Image (Packer) — Dựng cụm trong ~1–1.5 phút
- [ ] **Task 21 — Packer Templates (VMware & QEMU/Proxmox):** Tạo bộ template Packer bake sẵn OS tuning + binaries + OCI images kèm file đánh dấu `/etc/vkube-golden-image`; nâng cấp `pkg/engine` tự nhận diện `NodeFacts.HasGoldenImage` để bỏ qua bước tải/nạp artifact.

---

## Phase 9 (v0.5): Rolling Upgrade & DevSecOps Hardening
- [ ] **Task 22 — `vkube upgrade` & Hardening:** Implement rolling upgrade an toàn từng node qua `kubeadm upgrade`, bổ sung kiểm tra đồng bộ giờ (`chrony`/`systemd-timesyncd`) trong `preflight`, tùy chọn `metrics-server` dùng `serverTLSBootstrap` (thay `--kubelet-insecure-tls`) và bộ cấu hình CIS Benchmark cơ bản.

---

## Phase 10 (v1.0): Production-ready & Supply Chain Security
- [ ] **Task 23 — Ký Artifact (Cosign) & SBOM:** Tích hợp ký binary & Offline Bundle bằng Cosign, tự động sinh SBOM (Syft/SPDX) trong GitHub Actions release, hoàn thiện tài liệu vận hành Production.
