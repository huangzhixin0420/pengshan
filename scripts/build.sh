#!/usr/bin/env bash
# 交叉编译 release 资产：dist/pengshan-<goos>-<goarch>.tar.gz + checksums.txt。
# 用法：scripts/build.sh [version]   （version 默认 git describe，dev 构建 = dev）
set -euo pipefail

cd "$(dirname "$0")/.."
VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

rm -rf dist
mkdir -p dist

platforms=("darwin/arm64" "darwin/amd64" "linux/amd64" "linux/arm64")
for p in "${platforms[@]}"; do
  goos="${p%/*}"
  goarch="${p#*/}"
  out="dist/pengshan-${goos}-${goarch}"
  mkdir -p "$out"
  ldflags="-X github.com/huangzhixin0420/pengshan/internal/version.Version=${VERSION}
           -X github.com/huangzhixin0420/pengshan/internal/version.Commit=${COMMIT}
           -X github.com/huangzhixin0420/pengshan/internal/version.Date=${DATE}"
  for bin in pengshan pengshan-relay; do
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
      -trimpath -ldflags "$ldflags" -o "$out/$bin" "./cmd/$bin"
  done
  (cd "$out" && tar -czf "../pengshan-${goos}-${goarch}.tar.gz" pengshan pengshan-relay)
  rm -rf "$out"
  echo "built pengshan-${goos}-${goarch}.tar.gz (version ${VERSION})"
done

(cd dist && shasum -a 256 *.tar.gz > checksums.txt)
echo "checksums written"
