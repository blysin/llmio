#!/usr/bin/env bash
#
# build-image.sh — 构建 llmio Docker 镜像并推送到阿里云容器镜像服务 (ACR)
#
# 本脚本须在装有 Docker 的 WSL 中运行；Windows 侧请用同目录的 build-image.bat 包装器调用。
#
# 用法：
#   bash build-image.sh                 构建并把 <version> 与 latest 推送到远端（latest 为默认行为）
#   bash build-image.sh -v v0.8.17      指定版本号
#   bash build-image.sh --no-push       只构建 + 打本地 tag，不推送
#   bash build-image.sh --no-latest     推送 <version>，但不推送 latest
#   bash build-image.sh --no-cache      禁用 Docker 构建缓存
#   bash build-image.sh --dry-run       只展示执行计划，不实际构建
#
# 可通过环境变量覆盖：
#   LOCAL_IMAGE        本地镜像名     (默认 atopos31/llmio)
#   REGISTRY           仓库域名       (默认 registry.cn-hangzhou.aliyuncs.com)
#   NAMESPACE          命名空间       (默认 blysin-base)
#   IMAGE_NAME         远端镜像名     (默认 llmio)
#   PLATFORM           目标平台       (默认由 Docker 决定，例如 linux/amd64)
#   REGISTRY_USER      仓库用户名     (可选；与 REGISTRY_PASSWORD 同时设置才会自动登录)
#   REGISTRY_PASSWORD  仓库密码       (可选；切勿写进本文件，用 export 或命令行前缀传入)
#
# 示例：
#   bash build-image.sh
#   bash build-image.sh -v v0.8.17
#   REGISTRY_USER=me@example.com REGISTRY_PASSWORD=*** bash build-image.sh

set -euo pipefail

# ---------------------------------------------------------------------------
# 默认配置
# ---------------------------------------------------------------------------
LOCAL_IMAGE="${LOCAL_IMAGE:-atopos31/llmio}"
REGISTRY="${REGISTRY:-registry.cn-hangzhou.aliyuncs.com}"
NAMESPACE="${NAMESPACE:-blysin-base}"
IMAGE_NAME="${IMAGE_NAME:-llmio}"
REMOTE_IMAGE="${REGISTRY}/${NAMESPACE}/${IMAGE_NAME}"

# ---------------------------------------------------------------------------
# 切换到脚本所在目录（项目根），无论从何处调用
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")" && pwd)"
cd "$SCRIPT_DIR"

usage() {
  cat <<'EOF'
build-image.sh — 构建 llmio Docker 镜像并推送到阿里云 ACR

用法：
  bash build-image.sh [选项]

选项：
  -v, --version <vX.Y.Z>   指定镜像版本号（默认：复用本地最高 vX.Y.Z tag，不递增）
      --no-push            只构建并打本地 tag，不推送
      --no-latest          推送 <version> 但不推送 latest（默认会推送 latest）
      --push-latest        兼容旧调用，显式开启 latest 推送（默认已开启）
      --no-cache           禁用 Docker 构建缓存
      --dry-run            只展示执行计划，不实际构建
  -h, --help               显示本帮助

环境变量：
  LOCAL_IMAGE         本地镜像名（默认 atopos31/llmio）
  REGISTRY            仓库域名（默认 registry.cn-hangzhou.aliyuncs.com）
  NAMESPACE           命名空间（默认 blysin-base）
  IMAGE_NAME          远端镜像名（默认 llmio）
  PLATFORM            目标平台，如 linux/amd64（默认由 Docker 决定）
  REGISTRY_USER       仓库用户名（与 REGISTRY_PASSWORD 同时设置才会自动登录）
  REGISTRY_PASSWORD   仓库密码（切勿写进脚本，用 export 传入）

示例：
  bash build-image.sh
  bash build-image.sh -v v0.8.17
  REGISTRY_USER=me@example.com REGISTRY_PASSWORD=*** bash build-image.sh
EOF
}

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
VERSION=""
DO_PUSH=1
PUSH_LATEST=1
NO_CACHE=0
DRY_RUN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    -v|--version)
      if [[ $# -lt 2 ]]; then echo "错误: $1 需要一个参数" >&2; exit 2; fi
      VERSION="$2"; shift 2;;
    --no-push)     DO_PUSH=0; shift;;
    --no-latest)   PUSH_LATEST=0; shift;;
    --push-latest) PUSH_LATEST=1; shift;;
    --no-cache)    NO_CACHE=1; shift;;
    --dry-run)     DRY_RUN=1; shift;;
    -h|--help)     usage; exit 0;;
    *) echo "错误: 未知参数 '$1'（用 -h 查看用法）" >&2; exit 2;;
  esac
done

# ---------------------------------------------------------------------------
# 构建前同步远端 tag（--prune-tags 清理本地已删除的远端 tag）
# 必须放在「版本号推导」之前，否则版本推导仍会基于过期的本地 tag。
# 离线 / 无 remote / fetch 失败时仅告警，不阻断构建（沿用本地已有 tag）。
# ---------------------------------------------------------------------------
echo "[0/4] git fetch --tags --prune-tags ..."
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if git remote | grep -q .; then
    if git fetch --tags --prune-tags 2>/dev/null; then
      echo "      ✓ 已同步远端 tags"
    else
      echo "      ⚠ 同步远端 tags 失败（可能离线），沿用本地已有 tag" >&2
    fi
  else
    echo "      - 无 remote，跳过 fetch"
  fi
else
  echo "      - 非 git 仓库，跳过 fetch"
fi
echo

# ---------------------------------------------------------------------------
# 版本号推导：不传则直接复用本地最高 semver tag（不递增，可重复构建覆盖）
# ---------------------------------------------------------------------------
BASE_TAG=""
AUTO_DERIVED=0
if [[ -z "$VERSION" ]]; then
  BASE_TAG="$(git tag --sort=-v:refname 2>/dev/null | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n1 || true)"
  if [[ -z "$BASE_TAG" ]]; then
    echo "错误: 未找到形如 vX.Y.Z 的 git tag，无法自动推导版本号，请用 -v 手动指定。" >&2
    exit 2
  fi
  VERSION="$BASE_TAG"
  AUTO_DERIVED=1
fi

# ---------------------------------------------------------------------------
# 环境检查
# ---------------------------------------------------------------------------
if ! command -v docker >/dev/null 2>&1; then
  echo "错误: 找不到 docker 命令。本脚本须在安装了 Docker 的 WSL 中运行。" >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "错误: Docker 守护进程不可用（无权限或未启动）。" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 收集 git 信息
# ---------------------------------------------------------------------------
GIT_SHA="$(git rev-parse --short HEAD 2>/dev/null || true)"
GIT_BRANCH="$(git branch --show-current 2>/dev/null || true)"
if [[ -z "$GIT_SHA" ]]; then GIT_SHA="unknown"; fi
if [[ -z "$GIT_BRANCH" ]]; then GIT_BRANCH="detached"; fi

# ---------------------------------------------------------------------------
# 打印摘要
# ---------------------------------------------------------------------------
echo "========================================================"
echo " llmio Docker 镜像构建"
echo "========================================================"
printf " 项目根   : %s\n" "$SCRIPT_DIR"
printf " 构建源   : %s on %s\n" "$GIT_SHA" "$GIT_BRANCH"
if [[ "$AUTO_DERIVED" == 1 ]]; then
  printf " 版本号   : %s  (复用最新 tag)\n" "$VERSION"
else
  printf " 版本号   : %s  (手动指定)\n" "$VERSION"
fi
if [[ "$DO_PUSH" == 1 ]]; then
  printf " 本地镜像 : %s:%s , %s:latest\n" "$LOCAL_IMAGE" "$VERSION" "$LOCAL_IMAGE"
  printf " 远端镜像 : %s:%s" "$REMOTE_IMAGE" "$VERSION"
  if [[ "$PUSH_LATEST" == 1 ]]; then
    printf " , %s:latest\n" "$REMOTE_IMAGE"
  else
    printf "\n"
  fi
  printf " 推送     : 是"
  if [[ "$PUSH_LATEST" == 1 ]]; then printf " (含 latest)"; fi
  printf "\n"
else
  printf " 本地镜像 : %s:%s , %s:latest\n" "$LOCAL_IMAGE" "$VERSION" "$LOCAL_IMAGE"
  printf " 远端镜像 : %s:%s\n" "$REMOTE_IMAGE" "$VERSION"
  printf " 推送     : 否 (--no-push)\n"
fi
if [[ "$NO_CACHE" == 1 ]]; then echo " 缓存     : 已禁用 (--no-cache)"; fi
if [[ -n "${PLATFORM:-}" ]]; then echo " 目标平台 : $PLATFORM"; fi
echo "========================================================"

# 提醒（不阻塞执行）
if [[ -n "$(git status --porcelain 2>/dev/null || true)" ]]; then
  echo "⚠ 提示: 工作区有未提交改动，构建的是当前工作区内容而非 HEAD。"
fi
if git rev-parse -q --verify "refs/tags/${VERSION}" >/dev/null 2>&1; then
  echo "ℹ 提示: ${VERSION} 已存在，本次将覆盖同名镜像 tag（本地与远端）。"
fi
echo

if [[ "$DRY_RUN" == 1 ]]; then
  echo "（dry-run：仅展示执行计划，未执行任何 docker 命令）"
  exit 0
fi

# ---------------------------------------------------------------------------
# 组装 docker build 附加参数
# ---------------------------------------------------------------------------
DOCKER_BUILD_ARGS=()
if [[ -n "${PLATFORM:-}" ]]; then DOCKER_BUILD_ARGS+=(--platform "$PLATFORM"); fi
if [[ "$NO_CACHE" == 1 ]]; then DOCKER_BUILD_ARGS+=(--no-cache); fi

# ---------------------------------------------------------------------------
# 1) 构建
# ---------------------------------------------------------------------------
echo "[1/4] docker build ..."
docker build "${DOCKER_BUILD_ARGS[@]}" \
  --build-arg "VERSION=${VERSION}" \
  -t "${LOCAL_IMAGE}:${VERSION}" \
  -t "${LOCAL_IMAGE}:latest" \
  .
echo "      ✓ 构建完成"

# ---------------------------------------------------------------------------
# 2) 打远端 tag
# ---------------------------------------------------------------------------
echo "[2/4] docker tag ..."
docker tag "${LOCAL_IMAGE}:${VERSION}" "${REMOTE_IMAGE}:${VERSION}"
if [[ "$PUSH_LATEST" == 1 ]]; then
  docker tag "${LOCAL_IMAGE}:latest" "${REMOTE_IMAGE}:latest"
fi
echo "      ✓ ${REMOTE_IMAGE}:${VERSION}"

if [[ "$DO_PUSH" == 0 ]]; then
  echo
  echo "已跳过推送 (--no-push)。本地镜像可用：${LOCAL_IMAGE}:${VERSION}"
  exit 0
fi

# ---------------------------------------------------------------------------
# 3) 登录（仅当显式提供凭证；否则复用 ~/.docker/config.json）
# ---------------------------------------------------------------------------
echo "[3/4] docker login ..."
if [[ -n "${REGISTRY_USER:-}" && -n "${REGISTRY_PASSWORD:-}" ]]; then
  printf '%s' "$REGISTRY_PASSWORD" | docker login "$REGISTRY" --username "$REGISTRY_USER" --password-stdin
  echo "      ✓ 已登录 $REGISTRY (用户: $REGISTRY_USER)"
else
  echo "      - 未提供 REGISTRY_USER / REGISTRY_PASSWORD，复用已有凭证"
fi

# ---------------------------------------------------------------------------
# 4) 推送
# ---------------------------------------------------------------------------
echo "[4/4] docker push ..."
docker push "${REMOTE_IMAGE}:${VERSION}"
if [[ "$PUSH_LATEST" == 1 ]]; then
  docker push "${REMOTE_IMAGE}:latest"
fi

DIGEST="$(docker inspect --format='{{index .RepoDigests 0}}' "${REMOTE_IMAGE}:${VERSION}" 2>/dev/null || true)"

echo
echo "========================================================"
echo " ✓ 完成"
echo "   远端地址 : ${REMOTE_IMAGE}:${VERSION}"
if [[ "$PUSH_LATEST" == 1 ]]; then echo "   远端地址 : ${REMOTE_IMAGE}:latest"; fi
if [[ -n "$DIGEST" ]]; then echo "   Digest   : ${DIGEST}"; fi
echo "========================================================"
