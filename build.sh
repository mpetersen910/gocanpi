#!/usr/bin/env bash
set -euo pipefail
REG=192.168.50.1:5000
SHA=$(git rev-parse --short HEAD)
TAG="main-$SHA-$(date +%Y%m%d%H%M%S)"
docker buildx build --platform linux/arm64 --build-arg TARGET=./cmd/colcan -t $REG/gocanpi:$TAG --push .
docker buildx build --platform linux/arm64 --build-arg TARGET=./cmd/gencan -t $REG/gencan:$TAG --push .
echo "pushed $TAG"