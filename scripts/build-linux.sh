#!/usr/bin/env bash
set -euo pipefail
# 脚本位于 scripts/ 下，构建在仓库根目录进行
cd "$(dirname "$0")/.."
mkdir -p dist
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/rond-linux-amd64 .
echo "已生成 dist/rond-linux-amd64"
