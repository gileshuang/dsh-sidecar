#!/usr/bin/env bash
#
# 构建并部署 dsh-sidecar：二进制、示例配置、systemd 模板单元。
#
# 用法见 -h；典型用法：
#   sudo ./deploy/install.sh -u alice
#
set -euo pipefail

PREFIX=${PREFIX:-/opt/dsh-sidecar}
UNIT_NAME=dsh-sidecar@.service
UNIT_DIR=${UNIT_DIR:-/etc/systemd/system}
REPO_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

USER_NAME=""
DO_START=1

usage() {
    cat <<'EOF'
用法: sudo ./deploy/install.sh [选项]

  -u, --user 用户    中继运行身份（默认取 sudo 发起者，否则当前用户）
      --prefix 目录  安装前缀，默认 /opt/dsh-sidecar
      --no-start     只安装，不 enable/start
  -h, --help         显示本帮助
EOF
}

while [[ $# -gt 0 ]]; do
    case $1 in
        -u|--user)  USER_NAME=${2:?缺少用户名}; shift 2 ;;
        --prefix)   PREFIX=${2:?缺少目录}; shift 2 ;;
        --no-start) DO_START=0; shift ;;
        -h|--help)  usage; exit 0 ;;
        *) echo "未知参数: $1" >&2; usage; exit 2 ;;
    esac
done

if [[ ${EUID} -eq 0 ]]; then
    SUDO=()
else
    SUDO=(sudo)
fi

USER_NAME=${USER_NAME:-${SUDO_USER:-$(id -un)}}
if ! id -u "${USER_NAME}" >/dev/null 2>&1; then
    echo "用户不存在: ${USER_NAME}" >&2
    exit 1
fi
if [[ "${USER_NAME}" == root ]]; then
    echo "警告: 以 root 运行中继不推荐，建议用 -u 指定普通用户" >&2
fi

for cmd in go systemctl sed install; do
    command -v "${cmd}" >/dev/null 2>&1 || { echo "缺少命令: ${cmd}" >&2; exit 1; }
done

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

echo "==> 构建二进制"
(cd "${REPO_DIR}" && go build -mod=vendor -trimpath -ldflags "-s -w" -o "${work}/dsh-sidecar" .)

echo "==> 安装到 ${PREFIX}"
"${SUDO[@]}" install -Dm755 "${work}/dsh-sidecar" "${PREFIX}/dsh-sidecar"
"${SUDO[@]}" install -Dm644 "${REPO_DIR}/README.md" "${PREFIX}/README.md"
"${SUDO[@]}" install -Dm644 "${REPO_DIR}/deploy/config.example.yaml" "${PREFIX}/config.example.yaml"

echo "==> 渲染并安装 systemd 模板单元"
sed -e "s|@BINDIR@|${PREFIX}|g" -e "s|@DOCDIR@|${PREFIX}|g" \
        "${REPO_DIR}/deploy/systemd/${UNIT_NAME}" > "${work}/${UNIT_NAME}"
"${SUDO[@]}" install -Dm644 "${work}/${UNIT_NAME}" "${UNIT_DIR}/${UNIT_NAME}"
"${SUDO[@]}" systemctl daemon-reload

if [[ ${DO_START} -eq 1 ]]; then
    echo "==> 启用并启动 dsh-sidecar@${USER_NAME}"
    "${SUDO[@]}" systemctl enable "dsh-sidecar@${USER_NAME}.service"
    "${SUDO[@]}" systemctl restart "dsh-sidecar@${USER_NAME}.service"
fi

cat <<EOF

完成。常用命令：
  systemctl status  dsh-sidecar@${USER_NAME}
  journalctl -u dsh-sidecar@${USER_NAME} -f
  systemctl restart dsh-sidecar@${USER_NAME}
EOF
