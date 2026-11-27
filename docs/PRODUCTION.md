# Hướng dẫn Vận hành Production & Bảo mật Chuỗi Cung Ứng (`vkube`)

Tài liệu này tổng hợp các thực hành tốt nhất (Best Practices) khi triển khai và vận hành cụm Vanilla Kubernetes Production bằng `vkube` trong môi trường On-premise / Enterprise / Air-gapped.

---

## 1. Kiến trúc khuyến nghị cho Production

- **Control Plane HA:** Tối thiểu **3 Control Plane** (số lẻ để bảo vệ `etcd` Raft quorum) kèm **`vip`** (L2 ARP nếu cùng VLAN hoặc `spec.vipBGP` nếu khác VLAN L3). Giữ `allowSchedulingOnControlPlane: false`.
- **Bảo mật tầng mạng & truy cập:**
  - Dùng **SSH Bastion (`spec.ssh.bastion`)** cho các máy chủ nằm trong private subnet không mở SSH trực tiếp.
  - Khai báo mật khẩu/secret qua biến môi trường (`VKUBE_SSH_PASSWORD`, `VKUBE_SUDO_PASSWORD`, `VKUBE_BASTION_PASSWORD`, `VKUBE_S3_ACCESS_KEY`, `VKUBE_S3_SECRET_KEY`, `VKUBE_BACKUP_SFTP_PASSWORD`) thay vì lưu plaintext trong `cluster.yaml`.
  - Bật **CIS Hardening (`spec.controlPlane.cisHardening: true`)** để tự động kích hoạt mã hóa Secret trong `etcd` (AES-CBC Encryption at Rest), Audit Log, tắt `profiling`, `readOnlyPort: 0` và siết quyền file cấu hình `0600`/`0700`.
  - Bật **`spec.addons.metricsServer.secureTLS: true`** để dùng `kubelet serverTLSBootstrap` thay cho `--kubelet-insecure-tls`.
- **Đồng bộ thời gian:** Đảm bảo `chrony` hoặc `systemd-timesyncd` đồng bộ NTP trên toàn bộ nodes (tránh lệch giờ gây lỗi TLS và mất quorum `etcd`).

---

## 2. Xác thực chữ ký Cosign & SBOM (Supply Chain Security)

Mỗi bản phát hành `v*` của `vkube` trên GitHub Releases đều tự động đính kèm:
- `SHA256SUMS` và chữ ký Sigstore không khóa dài hạn `SHA256SUMS.sigstore.json` (Cosign OIDC).
- Danh sách thành phần phần mềm **SBOM** chuẩn SPDX (`vkube-<tag>.spdx.json`) kèm chữ ký `vkube-<tag>.spdx.json.sigstore.json`.

**Cách xác thực trước khi đưa binary vào vùng Air-gapped:**

```bash
TAG="v1.36.7-vkube.1"

# 1. Xác thực chữ ký Cosign của SHA256SUMS bằng danh tính GitHub Actions chính chủ
cosign verify-blob \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp "https://github.com/thaivu98/vkube/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  SHA256SUMS

# 2. Kiểm tra checksum binary khớp 100% với SHA256SUMS đã xác thực
sha256sum -c --ignore-missing SHA256SUMS

# 3. (Tùy chọn) Xác thực chữ ký SBOM SPDX
cosign verify-blob \
  --bundle "vkube-${TAG}.spdx.json.sigstore.json" \
  --certificate-identity-regexp "https://github.com/thaivu98/vkube/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "vkube-${TAG}.spdx.json"
```

---

## 3. Quy trình Vận hành Day-2 Chuẩn

### 3.1 Sao lưu `etcd` định kỳ & Diễn tập khôi phục (DR)
```bash
# Đẩy bản backup định kỳ lên S3 / MinIO và giữ lại 14 bản mới nhất
VKUBE_S3_ACCESS_KEY=... VKUBE_S3_SECRET_KEY=... \
  vkube backup -f prod.yaml --target s3 --retention 14

# Hoặc cài systemd timer tự động backup hằng ngày trên CP-1
vkube backup schedule -f prod.yaml --on-calendar "*-*-* 02:00:00" --retention 7

# Khôi phục cụm 1-node hoặc 3-node HA chống split-brain từ S3/MinIO:
VKUBE_S3_ACCESS_KEY=... VKUBE_S3_SECRET_KEY=... \
  vkube restore -f prod.yaml --backup s3://k8s-etcd-backups/prod/backup-prod-YYYYMMDD-HHMMSS.tar.gz --yes
```

### 3.2 Kiểm tra & Gia hạn chứng chỉ hàng năm
```bash
vkube certs check -f prod.yaml
vkube certs renew -f prod.yaml --yes
```

### 3.3 Mở rộng / Thu hẹp cụm & Rolling Upgrade
```bash
# Thêm node mới (sau khi thêm vào prod.yaml)
vkube add-node -f prod.yaml --bundle vkube-bundle-v1.36.7-vkube.1-linux-amd64.tar.zst

# Gỡ node an toàn (tự drain + gỡ etcd member nếu là CP)
vkube remove-node -f prod.yaml --node worker-4 --yes

# Rolling upgrade (luôn backup etcd trước khi nâng cấp!)
vkube backup -f prod.yaml
vkube upgrade -f prod.yaml --bundle vkube-bundle-v1.36.8-vkube.1-linux-amd64.tar.zst --yes
```

### 3.4 Tăng tốc triển khai xuống ~1–1.5 phút bằng Golden Image
Xem hướng dẫn chi tiết tại [packer/README.md](../packer/README.md) để bake sẵn VM Template cho VMware / Proxmox VE.
