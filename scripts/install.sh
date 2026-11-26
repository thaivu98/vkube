#!/usr/bin/env sh
# scripts/install.sh — Tải và cài đặt binary vkube mới nhất từ GitHub Releases (Linux & macOS).
# Cách dùng:
#   curl -fsSL https://raw.githubusercontent.com/thaivu98/vkube/v1.32.2-vkube.1/scripts/install.sh | sudo sh
#   # Hoặc cài bản mới nhất (latest):
#   VKUBE_VERSION=latest curl -fsSL https://raw.githubusercontent.com/thaivu98/vkube/main/scripts/install.sh | sudo sh

set -eu

REPO="thaivu98/vkube"
DEFAULT_VERSION="v1.32.2-vkube.1"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
REQUESTED_VERSION="${1:-${VKUBE_VERSION:-$DEFAULT_VERSION}}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  linux|darwin) ;;
  *)
    echo "Lỗi: Hệ điều hành không hỗ trợ qua script này ($OS). Hãy tải binary thủ công tại https://github.com/${REPO}/releases" >&2
    exit 1
    ;;
esac

RAW_ARCH="$(uname -m)"
case "$RAW_ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "Lỗi: Kiến trúc CPU không hỗ trợ ($RAW_ARCH)." >&2
    exit 1
    ;;
esac

if [ "$REQUESTED_VERSION" != "latest" ] && [ -n "$REQUESTED_VERSION" ]; then
  TAG="$REQUESTED_VERSION"
else
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name":' | head -n 1 | sed -E 's/.*"([^"]+)".*/\1/')"
  if [ -z "$TAG" ]; then
    echo "Lỗi: Chưa có bản Release nào trên GitHub (${REPO}). Bạn có thể build từ mã nguồn bằng 'make build'." >&2
    exit 1
  fi
fi

BIN_NAME="vkube-${OS}-${ARCH}"
URL="https://github.com/${REPO}/releases/download/${TAG}/${BIN_NAME}"
SHA_URL="https://github.com/${REPO}/releases/download/${TAG}/SHA256SUMS"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
TMP_FILE="${TMP_DIR}/${BIN_NAME}"
SHA_FILE="${TMP_DIR}/SHA256SUMS"

echo "Đang tải vkube ${TAG} (${OS}/${ARCH}) từ ${URL}..."
curl -fsSL "$URL" -o "$TMP_FILE"

echo "Đang xác thực SHA256SUMS..."
curl -fsSL "$SHA_URL" -o "$SHA_FILE"
EXPECTED_SHA="$(awk -v f="$BIN_NAME" '$2 == f { print $1; exit }' "$SHA_FILE")"
if [ -z "$EXPECTED_SHA" ]; then
  echo "Lỗi: Không tìm thấy checksum cho ${BIN_NAME} trong SHA256SUMS." >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL_SHA="$(sha256sum "$TMP_FILE" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL_SHA="$(shasum -a 256 "$TMP_FILE" | awk '{print $1}')"
else
  echo "Lỗi: Không tìm thấy sha256sum hoặc shasum để kiểm tra toàn vẹn binary." >&2
  exit 1
fi

if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
  echo "Lỗi: SHA256 không khớp cho ${BIN_NAME}! Expected=${EXPECTED_SHA}, Got=${ACTUAL_SHA}" >&2
  exit 1
fi
echo "Xác thực SHA256 OK (${ACTUAL_SHA})"

chmod +x "$TMP_FILE"
mkdir -p "$INSTALL_DIR"
mv "$TMP_FILE" "${INSTALL_DIR}/vkube"

echo "Đã cài đặt thành công tại ${INSTALL_DIR}/vkube"
echo ""
"${INSTALL_DIR}/vkube" --help
echo ""
echo "=================================================================="
echo " Bắt đầu nhanh (Chế độ Online — không cần build bundle):"
echo "   1. Tạo file cấu hình mẫu cluster.yaml đầy đủ hướng dẫn:"
echo "        vkube init-config -o cluster.yaml"
echo "      (Thêm --ha nếu muốn dựng cụm HA 3 Control Planes)"
echo "   2. Chỉnh IP/SSH trong cluster.yaml rồi triển khai ngay:"
echo "        vkube apply -f cluster.yaml"
echo ""
echo " (Nếu triển khai môi trường Air-gapped không Internet:"
echo "   vkube bundle build --arch amd64"
echo "   vkube apply -f cluster.yaml --bundle bundles/vkube-bundle-*.tar.zst)"
echo "=================================================================="
