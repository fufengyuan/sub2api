#!/bin/sh
# 固定输出位置的 Linux amd64 后端打包脚本。
# 产物固定写入 backend/dist/server-linux-amd64，避免临时命令导致位置/命名漂移。
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BACKEND_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
OUT_DIR="$(CDPATH= cd -- "$BACKEND_DIR/dist" 2>/dev/null && pwd || printf '%s' "$BACKEND_DIR/dist")"
OUT="$OUT_DIR/server-linux-amd64"
VERSION="$(sh "$SCRIPT_DIR/resolve-version.sh")"

mkdir -p "$OUT_DIR"

# 用户可在环境变量中覆盖输出路径（仍是显式统一入口），默认固定 dist/。
if [ -n "${SUB2API_OUT:-}" ]; then
  OUT="$SUB2API_OUT"
fi

echo "==> building linux/amd64 server"
echo "    version=$VERSION"
echo "    output=$OUT"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build \
  -tags embed \
  -ldflags="-s -w -X main.Version=$VERSION" \
  -trimpath \
  -o "$OUT" \
  ./cmd/server

echo "==> done: $(ls -lh "$OUT" | awk '{print $5}' )  $OUT"