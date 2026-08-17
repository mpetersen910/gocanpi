#!/usr/bin/env bash
set -euo pipefail
REG=192.168.50.1:5000
SHA=$(git describe --always --dirty)
docker buildx build --platform linux/arm64 --build-arg TARGET=./cmd/colcan -t $REG/gocanpi:$SHA --push .
docker buildx build --platform linux/arm64 --build-arg TARGET=./cmd/gencan -t $REG/gencan:$SHA --push .
echo "pushed $SHA"