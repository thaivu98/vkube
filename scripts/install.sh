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

TMP_FILE="$(mktemp)"
trap 'rm -f "$TMP_FILE"' EXIT

echo "Đang tải vkube ${TAG} (${OS}/${ARCH}) từ ${URL}..."
curl -fsSL "$URL" -o "$TMP_FILE"
chmod +x "$TMP_FILE"

mkdir -p "$INSTALL_DIR"
mv "$TMP_FILE" "${INSTALL_DIR}/vkube"

echo "Đã cài đặt thành công tại ${INSTALL_DIR}/vkube"
"${INSTALL_DIR}/vkube" version
