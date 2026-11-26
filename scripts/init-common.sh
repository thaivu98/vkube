#!/usr/bin/env bash
# scripts/init-common.sh — Kiểm tra và cài đặt các công cụ tiên quyết chung để build/phát triển vkube từ mã nguồn.
# Lưu ý: Người dùng cuối tải binary vkube có sẵn KHÔNG cần cài bất kỳ công cụ nào trong số này.
# Hỗ trợ: macOS (Homebrew) và Ubuntu/Debian Linux (apt + Go official tarball).

set -euo pipefail

MIN_GO_VERSION="1.23"
INSTALL_GO_VERSION="1.23.6"

log_info()  { printf "\033[1;34m[INFO]\033[0m  %s\n" "$*"; }
log_ok()    { printf "\033[1;32m[OK]\033[0m    %s\n" "$*"; }
log_warn()  { printf "\033[1;33m[WARN]\033[0m  %s\n" "$*"; }
log_err()   { printf "\033[1;31m[ERROR]\033[0m %s\n" "$*" >&2; }

detect_os() {
    local uname_s
    uname_s="$(uname -s)"
    case "$uname_s" in
        Darwin) echo "darwin" ;;
        Linux)  echo "linux" ;;
        *)
            log_err "Hệ điều hành không hỗ trợ tự động cài qua script này: $uname_s"
            log_err "Trên Windows, vui lòng cài Go >= ${MIN_GO_VERSION} từ https://go.dev/dl/ và Git."
            exit 1
            ;;
    esac
}

detect_arch() {
    local uname_m
    uname_m="$(uname -m)"
    case "$uname_m" in
        x86_64|amd64)  echo "amd64" ;;
        arm64|aarch64) echo "arm64" ;;
        *)
            log_err "Kiến trúc CPU không hỗ trợ: $uname_m"
            exit 1
            ;;
    esac
}

version_ge() {
    # Trả về 0 nếu $1 >= $2 (so sánh major.minor)
    local v1_major v1_minor v2_major v2_minor
    v1_major="$(echo "$1" | cut -d. -f1)"
    v1_minor="$(echo "$1" | cut -d. -f2)"
    v2_major="$(echo "$2" | cut -d. -f1)"
    v2_minor="$(echo "$2" | cut -d. -f2)"
    if [ "$v1_major" -gt "$v2_major" ]; then return 0; fi
    if [ "$v1_major" -eq "$v2_major" ] && [ "$v1_minor" -ge "$v2_minor" ]; then return 0; fi
    return 1
}

check_go() {
    if command -v go >/dev/null 2>&1; then
        local current_ver
        current_ver="$(go version | awk '{print $3}' | sed 's/^go//')"
        if version_ge "$current_ver" "$MIN_GO_VERSION"; then
            log_ok "Go đã sẵn sàng: go${current_ver} ($(command -v go))"
            return 0
        else
            log_warn "Go hiện tại là go${current_ver} (< ${MIN_GO_VERSION}), cần nâng cấp."
            return 1
        fi
    fi
    return 1
}

install_macos() {
    if ! command -v brew >/dev/null 2>&1; then
        log_err "Chưa cài Homebrew. Vui lòng cài Homebrew trước: https://brew.sh"
        exit 1
    fi

    for pkg in git make; do
        if command -v "$pkg" >/dev/null 2>&1; then
            log_ok "$pkg đã sẵn sàng"
        else
            log_info "Đang cài đặt $pkg qua Homebrew..."
            brew install "$pkg"
        fi
    done

    if ! check_go; then
        log_info "Đang cài đặt Go qua Homebrew..."
        brew install go
    fi
}

install_linux() {
    local arch="$1"
    if command -v apt-get >/dev/null 2>&1; then
        log_info "Kiểm tra git, make, curl, ca-certificates qua apt..."
        local missing=()
        for cmd in git make curl; do
            command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
        done
        if [ "${#missing[@]}" -gt 0 ]; then
            sudo apt-get update -qq
            sudo apt-get install -y -qq "${missing[@]}" ca-certificates
        else
            log_ok "git, make, curl đã sẵn sàng"
        fi
    else
        log_warn "Không phát hiện apt-get. Đảm bảo bạn đã cài sẵn git, make và curl."
    fi

    if ! check_go; then
        local tarball="go${INSTALL_GO_VERSION}.linux-${arch}.tar.gz"
        local url="https://go.dev/dl/${tarball}"
        log_info "Đang tải Go ${INSTALL_GO_VERSION} (${arch}) từ ${url}..."
        curl -fSL "$url" -o "/tmp/${tarball}"
        sudo rm -rf /usr/local/go
        sudo tar -C /usr/local -xzf "/tmp/${tarball}"
        rm -f "/tmp/${tarball}"
        export PATH="/usr/local/go/bin:${PATH}"
        log_warn "Đã cài Go vào /usr/local/go. Hãy thêm dòng sau vào ~/.bashrc hoặc ~/.zshrc nếu chưa có:"
        echo '  export PATH="/usr/local/go/bin:$PATH"'
    fi
}

main() {
    local os arch
    os="$(detect_os)"
    arch="$(detect_arch)"
    log_info "Khởi tạo môi trường phát triển vkube trên ${os}/${arch}..."

    if [ "$os" = "darwin" ]; then
        install_macos
    else
        install_linux "$arch"
    fi

    echo ""
    log_ok "Hoàn tất kiểm tra công cụ tiên quyết:"
    echo "  - git:  $(git --version)"
    echo "  - make: $(make --version | head -n 1)"
    echo "  - go:   $(go version)"
    echo ""
    log_info "Bạn có thể chạy 'make build' hoặc 'make test' ngay bây giờ."
}

main "$@"
