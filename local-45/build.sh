#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
VERSION="${1:?Usage: bash local-45/build.sh VERSION (without v)}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || { printf 'Invalid version\n' >&2; exit 1; }
mkdir -p local-45/dist
# 必须构建整个包，不能只传 main.go，否则会漏掉 stream.go。
go test -race ./cmd/wb-router
go vet ./cmd/wb-router
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.appVersion=$VERSION" -o local-45/dist/wb2api ./cmd/server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o local-45/dist/wb-router ./cmd/wb-router
printf '%s\n' "$VERSION" > local-45/dist/VERSION
(cd local-45/dist && shasum -a 256 wb2api wb-router > SHA256SUMS)
printf 'Built version %s in local-45/dist\n' "$VERSION"
