#!/usr/bin/env bash
#
# 卸载 dsh-sidecar：停止并禁用实例、移除 systemd 模板单元，可选删除安装目录。
#
# 用法见 -h；典型用法：
#   sudo ./deploy/uninstall.sh -u alice
#   sudo ./deploy/uninstall.sh -u alice --purge
#
set -euo pipefail

PREFIX=${PREFIX:-/opt/dsh-sidecar}
UNIT_NAME=dsh-sidecar@.service
UNIT_DIR=${UNIT_DIR:-/etc/systemd/system}

USER_NAME=""
PURGE=0
FORCE=0

usage() {
    cat <<'EOF'
用法: sudo ./deploy/uninstall.sh [选项]

  -u, --user 用户    要卸载的实例身份（默认取 sudo 发起者，否则当前用户）
      --purge        同时删除安装目录（默认保留二进制）
      --force        即使还有其它实例，也移除模板单元与 drop-in
  -h, --help         显示本帮助
EOF
}

while [[ $# -gt 0 ]]; do
    case $1 in
        -u|--user) USER_NAME=${2:?缺少用户名}; shift 2 ;;
        --purge)   PURGE=1; shift ;;
        --force)   FORCE=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "未知参数: $1" >&2; usage; exit 2 ;;
    esac
done

if [[ ${EUID} -eq 0 ]]; then
    SUDO=()
else
    SUDO=(sudo)
fi

USER_NAME=${USER_NAME:-${SUDO_USER:-$(id -un)}}

echo "==> 停止并禁用 dsh-sidecar@${USER_NAME}"
"${SUDO[@]}" systemctl disable --now "dsh-sidecar@${USER_NAME}.service" >/dev/null 2>&1 || true

# 判断是否还有别的实例在用同一个模板单元
others=$(
    "${SUDO[@]}" systemctl list-units --type=service --all --no-legend 'dsh-sidecar@*' 2>/dev/null \
        | awk '{print $1}' \
        | grep -v "^dsh-sidecar@${USER_NAME}\.service$" \
        | grep -v '^$' \
        || true
)

if [[ -z "${others}" || ${FORCE} -eq 1 ]]; then
    echo "==> 移除模板单元 ${UNIT_DIR}/${UNIT_NAME}"
    "${SUDO[@]}" rm -f "${UNIT_DIR}/${UNIT_NAME}"
    "${SUDO[@]}" systemctl daemon-reload

else
    cat <<EOF
仍有其它实例使用该模板单元，已保留 ${UNIT_DIR}/${UNIT_NAME}：
${others}
如需强制移除，重跑并加 --force。
EOF
fi

if [[ ${PURGE} -eq 1 ]]; then
    echo "==> 删除安装目录 ${PREFIX}"
    "${SUDO[@]}" rm -rf "${PREFIX}"
else
    echo "保留 ${PREFIX}（如需一并删除，重跑并加 --purge）"
fi

echo
echo "完成。"
