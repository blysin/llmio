#!/bin/bash
# ============================================================================
#  build-image.command - macOS 包装器，转发给同目录的 build-image.sh
#
#  用法：
#    ./build-image.command                  复用最新 tag 版本（不递增）
#    ./build-image.command -v v0.8.17       指定版本号
#    ./build-image.command --no-push        只构建 + 打本地 tag，不推送
#    ./build-image.command --no-latest      推送 <version> 但不推送 latest（latest 默认推送）
#    ./build-image.command --no-cache       禁用 Docker 构建缓存
#    ./build-image.command --dry-run        只展示执行计划
#
#  用 LLMIO_DOCKER_CONTEXT 覆盖 Docker 上下文（默认 colima）。
#  在 Finder 里双击本文件即可运行（.command 会拉起 Terminal）。
# ============================================================================

# 强制 UTF-8，保证脚本中文输出不乱码（对应 Windows 侧的 chcp 65001）。
export LANG="${LANG:-en_US.UTF-8}"
export LC_ALL="${LC_ALL:-en_US.UTF-8}"

CTX="${LLMIO_DOCKER_CONTEXT:-colima}"

# 从脚本所在目录（项目根）调用，无论从哪里双击或执行。
cd "$(dirname "$(readlink -f "$0")")" || exit 1

# latest 现已是默认推送行为，无需再显式传 --push-latest；如需跳过用 --no-latest。
DOCKER_CONTEXT="$CTX" bash ./build-image.sh "$@"
RC=$?

# 双击启动（无参数）时保留窗口，便于查看结果。
if [ $# -eq 0 ]; then
  printf '\n[exit code %s]\n' "$RC"
  printf '按回车键关闭本窗口...\n'
  read -r _
fi

exit $RC
