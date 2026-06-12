#!/usr/bin/env bash
# Build + push the naozhi cloud-sandbox base image (Phase 0 spike).
set -euo pipefail

REGION=us-west-2
ACCOUNT=788668107894
REPO=naozhi-sandbox
TAG="${1:-phase0}"
ECR="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com"
DIR="$(cd "$(dirname "$0")" && pwd)"

# Stage the host claude CLI binary into the build context (ARM64 glibc ELF).
CLAUDE_BIN="$(readlink -f "$(which claude)")"
cp -f "$CLAUDE_BIN" "$DIR/claude"

aws ecr describe-repositories --repository-names "$REPO" --region "$REGION" >/dev/null 2>&1 \
  || aws ecr create-repository --repository-name "$REPO" --region "$REGION" >/dev/null

aws ecr get-login-password --region "$REGION" \
  | docker login --username AWS --password-stdin "$ECR"

# Stamp the image version (RFC §7.3 run-record meta) = the pushed tag, so a
# run record records which image produced it.
docker build --build-arg "IMAGE_VERSION=$TAG" -t "$ECR/$REPO:$TAG" "$DIR"
docker push "$ECR/$REPO:$TAG"
echo "pushed: $ECR/$REPO:$TAG"
