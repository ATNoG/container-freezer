#!/bin/bash
set -e

REGISTRY=ghcr.io/pmacoutinho
TAG=${1:-latest}

docker buildx build \
  --platform linux/amd64 \
  --builder mec-builder \
  -t ${REGISTRY}/freezer-daemon:${TAG} \
  --push \
  .

echo "Pushed ${REGISTRY}/freezer-daemon:${TAG}"
