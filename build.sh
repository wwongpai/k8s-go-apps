#!/usr/bin/env bash
# build.sh — Build and push multi-arch Docker images for all insurance microservices.
#
# Prerequisites:
#   - Docker Desktop with buildx support
#   - Logged in to Docker Hub: docker login
#
# Usage:
#   chmod +x build.sh
#   ./build.sh
#
# Environment overrides (optional):
#   REGISTRY  — Docker Hub username / registry prefix (default: wwongpai)
#   VERSION   — Image tag (default: 1.0.0)

set -euo pipefail

REGISTRY="${REGISTRY:-wwongpai}"
VERSION="${VERSION:-1.0.0}"

SERVICES=(
  policy-service
  claims-service
  customer-service
  premium-calculator-service
  notification-service
  api-gateway
)

# ---------------------------------------------------------------------------
# Helper
# ---------------------------------------------------------------------------

log() {
  echo "[$(date '+%H:%M:%S')] $*"
}

# ---------------------------------------------------------------------------
# Step 1: Set up Docker buildx
# ---------------------------------------------------------------------------

log "=== Setting up Docker buildx ==="

if docker buildx inspect multiarch-builder >/dev/null 2>&1; then
  log "Reusing existing builder: multiarch-builder"
  docker buildx use multiarch-builder
else
  log "Creating new builder: multiarch-builder"
  docker buildx create --use --name multiarch-builder
fi

docker buildx inspect --bootstrap
log "Builder ready."

# ---------------------------------------------------------------------------
# Step 2: Build Go microservices
# ---------------------------------------------------------------------------

log "=== Building Go microservices ==="

for svc in "${SERVICES[@]}"; do
  log "--- Building ${svc} ---"

  SERVICE_DIR="./services/${svc}"
  if [[ ! -d "${SERVICE_DIR}" ]]; then
    echo "ERROR: Service directory not found: ${SERVICE_DIR}" >&2
    exit 1
  fi

  docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --tag "${REGISTRY}/insurance-${svc}:${VERSION}" \
    --push \
    "${SERVICE_DIR}"

  log "${svc} pushed as ${REGISTRY}/insurance-${svc}:${VERSION}"
done

# ---------------------------------------------------------------------------
# Step 3: Build web frontend
# ---------------------------------------------------------------------------

log "--- Building web-app ---"

WEB_DIR="./services/web-app"
if [[ ! -d "${WEB_DIR}" ]]; then
  echo "ERROR: Service directory not found: ${WEB_DIR}" >&2
  exit 1
fi

docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --tag "${REGISTRY}/insurance-web-app:${VERSION}" \
  --push \
  "${WEB_DIR}"

log "web-app pushed as ${REGISTRY}/insurance-web-app:${VERSION}"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

log "=== Build complete! ==="
echo ""
echo "Images pushed:"
for svc in "${SERVICES[@]}" web-app; do
  echo "  ${REGISTRY}/insurance-${svc}:${VERSION}"
done
echo ""
echo "Verify multi-arch manifests with:"
echo "  docker buildx imagetools inspect ${REGISTRY}/insurance-policy-service:${VERSION}"
