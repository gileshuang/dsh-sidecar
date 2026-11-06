# Maintainer: alice <alice@example.com>
#
# 构建：makepkg -f
# 安装：sudo pacman -U dsh-sidecar-<ver>-<rel>-<arch>.pkg.tar.zst
#
# 源码来自本目录（makepkg 在 PKGBUILD 所在处运行），依赖已 vendor，构建不需要联网。

pkgname=dsh-sidecar
pkgver=0.1.0
pkgrel=1
pkgdesc="Loopback relay that lets a remote browser use dsh web and the ReMe status page"
arch=('x86_64' 'aarch64')
license=('MIT')
depends=()
makedepends=('go')
# 二进制已用 -ldflags "-s -w" 处理；再 strip 可能破坏 Go 的符号表
options=('!strip' '!lto')
source=()
sha256sums=()

# PKGBUILD 所在目录（绝对路径），build() 从这里取源码
_srcdir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

build() {
    export CGO_ENABLED=0
    export GOFLAGS="-mod=vendor -buildvcs=false"
    export GOCACHE="${srcdir}/.gocache"
    export GOPATH="${srcdir}/.gopath"

    install -d "${srcdir}/src"
    cp -a "${_srcdir}/main.go" \
          "${_srcdir}/go.mod" \
          "${_srcdir}/go.sum" \
          "${_srcdir}/vendor" \
          "${_srcdir}/deploy" \
          "${_srcdir}/README.md" \
          "${_srcdir}/LICENSE" \
          "${srcdir}/src/"

    cd "${srcdir}/src"
    go build -trimpath -ldflags "-s -w" -o dsh-sidecar .
}

package() {
    install -Dm755 "${srcdir}/src/dsh-sidecar" "${pkgdir}/usr/bin/dsh-sidecar"

    # 单元模板里的占位符换成包内路径
    sed -e 's|@BINDIR@|/usr/bin|g' \
        -e 's|@DOCDIR@|/usr/share/doc/dsh-sidecar|g' \
        "${srcdir}/src/deploy/systemd/dsh-sidecar@.service" \
        > "${srcdir}/dsh-sidecar@.service"
    install -Dm644 "${srcdir}/dsh-sidecar@.service" \
        "${pkgdir}/usr/lib/systemd/system/dsh-sidecar@.service"

    install -Dm644 "${srcdir}/src/README.md" \
        "${pkgdir}/usr/share/doc/dsh-sidecar/README.md"
    install -Dm644 "${srcdir}/src/deploy/config.example.yaml" \
        "${pkgdir}/usr/share/doc/dsh-sidecar/config.example.yaml"
    install -Dm644 "${srcdir}/src/LICENSE" \
        "${pkgdir}/usr/share/licenses/dsh-sidecar/LICENSE"
}
