#!/usr/bin/env bash
# 蓬山一键安装脚本：curl -fsSL https://raw.githubusercontent.com/huangzhixin0420/pengshan/main/scripts/install.sh | bash
#
# 流程：探测平台 → 从 GH Release（或 RELEASES_URL 覆盖，本地演练用）拉
# pengshan-<goos>-<goarch>.tar.gz + checksums.txt → sha256 校验 →
# 解包到 ~/.pengshan/bin/ → PATH 提示 → HERMES_PENGSHAN_AUTOSTART=1 时启用自启 → doctor。
#
# 环境变量：
#   PENGSHAN_VERSION   指定版本（默认 latest）
#   RELEASES_URL       资产基 URL 覆盖（默认 https://github.com/huangzhixin0420/pengshan/releases）
#                      本地演练：file:///path/to/dist（目录内含 tar.gz + checksums.txt）
#   HERMES_PENGSHAN_AUTOSTART=1   安装后启用开机自启
set -euo pipefail

REPO="huangzhixin0420/pengshan"
VERSION="${PENGSHAN_VERSION:-latest}"
RELEASES_URL="${RELEASES_URL:-https://github.com/${REPO}/releases}"
INSTALL_DIR="${HOME}/.pengshan/bin"

log()  { printf '\033[1;36m[pengshan]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[pengshan]\033[0m %s\n' "$*" >&2; exit 1; }

# --- 平台探测 ---
goos="$(uname -s | tr '[:upper:]' '[:lower:]')"
goarch="$(uname -m)"
case "$goarch" in
  arm64|aarch64) goarch="arm64" ;;
  x86_64|amd64)  goarch="amd64" ;;
  *) fail "不支持的架构: $goarch" ;;
esac
case "$goos" in
  darwin|linux) ;;
  *) fail "不支持的平台: $goos" ;;
esac
ASSET="pengshan-${goos}-${goarch}.tar.gz"
log "平台：${goos}/${goarch}，资产：${ASSET}，版本：${VERSION}"

# --- 解析下载地址 ---
if [ "${RELEASES_URL#file://}" != "${RELEASES_URL}" ]; then
  # 本地目录模式（演练/离线）：目录内直接取文件。
  LOCAL_DIR="${RELEASES_URL#file://}"
  [ -f "${LOCAL_DIR}/${ASSET}" ]     || fail "本地目录缺 ${ASSET}"
  [ -f "${LOCAL_DIR}/checksums.txt" ] || fail "本地目录缺 checksums.txt"
  ASSET_URL="file://${LOCAL_DIR}/${ASSET}"
  SUMS_URL="file://${LOCAL_DIR}/checksums.txt"
  # latest → 从目录内版本号文件或 tar 内二进制读（演练时直接猜一个即可）。
  if [ "$VERSION" = "latest" ]; then
    VERSION="$(tar -xzOf "${LOCAL_DIR}/${ASSET}" pengshan 2>/dev/null | strings | grep -m1 -oE 'v[0-9]+\.[0-9]+\.[0-9]+[^ ]*' || echo local)"
  fi
else
  if [ "$VERSION" = "latest" ]; then
    API_URL="https://api.github.com/repos/${REPO}/releases/latest"
  else
    API_URL="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
  fi
  log "查询 release：${API_URL}"
  JSON="$(curl -fsSL -H 'Accept: application/vnd.github+json' "$API_URL")" || fail "查询 release 失败（网络或版本不存在）"
  TAG="$(printf '%s' "$JSON" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
  [ -n "$TAG" ] || fail "release 无 tag"
  VERSION="$TAG"
  ASSET_URL="$(printf '%s' "$JSON" | grep -E '"browser_download_url"[^"]*"[^"]*'"${ASSET//./\\.}" | sed -E 's/.*"browser_download_url"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' | head -1)"
  SUMS_URL="$(printf '%s' "$JSON" | grep -E '"browser_download_url"[^"]*"[^"]*checksums\.txt' | sed -E 's/.*"browser_download_url"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' | head -1)"
  [ -n "$ASSET_URL" ] || fail "release ${VERSION} 无资产 ${ASSET}"
  [ -n "$SUMS_URL" ]  || fail "release ${VERSION} 缺 checksums.txt"
fi
log "目标版本：${VERSION}"

# --- 下载 + 校验 ---
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
log "下载 ${ASSET}"
curl -fsSL "$ASSET_URL" -o "${TMP}/${ASSET}" || fail "下载失败：${ASSET_URL}"
curl -fsSL "$SUMS_URL"  -o "${TMP}/checksums.txt" || fail "下载失败：${SUMS_URL}"

log "校验 sha256"
EXPECTED="$(grep " ${ASSET}\$" "${TMP}/checksums.txt" | cut -d' ' -f1)"
[ -n "$EXPECTED" ] || fail "checksums.txt 无 ${ASSET} 条目"
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMP}/${ASSET}" | cut -d' ' -f1)"
else
  ACTUAL="$(shasum -a 256 "${TMP}/${ASSET}" | cut -d' ' -f1)"
fi
[ "$EXPECTED" = "$ACTUAL" ] || fail "sha256 不符（期望 ${EXPECTED}，实际 ${ACTUAL}）——已中止"
log "校验通过"

# --- 安装 ---
mkdir -p "$INSTALL_DIR"
chmod 700 "${HOME}/.pengshan" 2>/dev/null || true
tar -xzf "${TMP}/${ASSET}" -C "$INSTALL_DIR" pengshan pengshan-relay
chmod 755 "${INSTALL_DIR}/pengshan" "${INSTALL_DIR}/pengshan-relay"
log "已安装到 ${INSTALL_DIR}"
"${INSTALL_DIR}/pengshan" version

# --- PATH ---
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    SHELL_RC=""
    case "${SHELL##*/}" in
      zsh)  SHELL_RC="${HOME}/.zshrc" ;;
      bash) SHELL_RC="${HOME}/.bashrc" ;;
    esac
    if [ -n "$SHELL_RC" ]; then
      echo "export PATH=\"\$HOME/.pengshan/bin:\$PATH\"" >> "$SHELL_RC"
      log "已写入 PATH 到 ${SHELL_RC}（新开终端生效；当前会话可先用全路径）"
    else
      log "请手动把 ${INSTALL_DIR} 加入 PATH"
    fi
    ;;
esac

# --- 可选自启 ---
if [ "${HERMES_PENGSHAN_AUTOSTART:-0}" = "1" ]; then
  log "启用开机自启"
  "${INSTALL_DIR}/pengshan" autostart on || fail "autostart on 失败"
fi

log "下一步："
echo "  1) pengshan config set serve_addr 127.0.0.1:9121        # 你的 hermes serve 地址"
echo "  2) pengshan config set relay_urls '[\"ws://你的relay:9400\"]'"
echo "  3) pengshan pair                                        # 生成二维码给青鸟扫"
echo "  4) pengshan doctor                                      # 体检"
