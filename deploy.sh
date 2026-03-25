#!/usr/bin/env bash
# deploy.sh — Deploy the insurance microservices application to a GKE cluster.
#
# Secrets are read from environment variables — never from files or arguments.
# The Datadog Agent is deployed before application pods so the Admission Controller
# webhook is registered before any pod mutations are needed.
#
# Prerequisites:
#   - kubectl configured with correct context (warach-gke-go-app)
#   - helm v3 installed
#   - docker buildx images already pushed (run ./build.sh first)
#   - datadog-values.yaml present in project root
#   - k8s/ manifests present
#
# Required environment variables:
#   DD_API_KEY         — Datadog API key
#   DD_APP_KEY         — Datadog Application key
#   POSTGRES_PASSWORD  — Password for the insurance PostgreSQL database
#   REDIS_PASSWORD     — Password for the Redis instance
#
# Usage:
#   chmod +x deploy.sh
#   export DD_API_KEY="..."
#   export DD_APP_KEY="..."
#   export POSTGRES_PASSWORD="..."
#   export REDIS_PASSWORD="..."
#   ./deploy.sh

set -euo pipefail

# ---------------------------------------------------------------------------
# Validate required environment variables
# The := syntax prints a descriptive error and exits if the var is unset or empty.
# ---------------------------------------------------------------------------

: "${DD_API_KEY:?DD_API_KEY must be set (export DD_API_KEY=<your-datadog-api-key>)}"
: "${DD_APP_KEY:?DD_APP_KEY must be set (export DD_APP_KEY=<your-datadog-app-key>)}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set (export POSTGRES_PASSWORD=<strong-password>)}"
: "${REDIS_PASSWORD:?REDIS_PASSWORD must be set (export REDIS_PASSWORD=<strong-password>)}"

# ---------------------------------------------------------------------------
# Helper
# ---------------------------------------------------------------------------

log() {
  echo ""
  echo "[$(date '+%H:%M:%S')] $*"
}

# ---------------------------------------------------------------------------
# Step 1: Create namespaces
# ---------------------------------------------------------------------------

log "=== Step 1: Create namespaces ==="

kubectl apply -f k8s/namespace.yaml

# Create datadog namespace idempotently
kubectl create namespace datadog --dry-run=client -o yaml | kubectl apply -f -

echo "Namespaces ready."

# ---------------------------------------------------------------------------
# Step 2: Create secrets
# ---------------------------------------------------------------------------

log "=== Step 2: Create secrets ==="

# --- insurance-app namespace ---

kubectl create secret generic datadog-secret \
  --from-literal=api-key="${DD_API_KEY}" \
  --from-literal=app-key="${DD_APP_KEY}" \
  -n insurance-app \
  --dry-run=client -o yaml | kubectl apply -f -
echo "  insurance-app/datadog-secret: ok"

kubectl create secret generic postgres-secret \
  --from-literal=password="${POSTGRES_PASSWORD}" \
  -n insurance-app \
  --dry-run=client -o yaml | kubectl apply -f -
echo "  insurance-app/postgres-secret: ok"

kubectl create secret generic redis-secret \
  --from-literal=password="${REDIS_PASSWORD}" \
  -n insurance-app \
  --dry-run=client -o yaml | kubectl apply -f -
echo "  insurance-app/redis-secret: ok"

# --- datadog namespace ---

kubectl create secret generic datadog-secret \
  --from-literal=api-key="${DD_API_KEY}" \
  --from-literal=app-key="${DD_APP_KEY}" \
  -n datadog \
  --dry-run=client -o yaml | kubectl apply -f -
echo "  datadog/datadog-secret: ok"

# ---------------------------------------------------------------------------
# Step 3: Deploy Datadog Agent
# IMPORTANT: Must happen BEFORE application pods are created so the Admission
# Controller webhook is registered when pod mutation is needed.
# ---------------------------------------------------------------------------

log "=== Step 3: Deploy Datadog Agent ==="

helm repo add datadog https://helm.datadoghq.com 2>/dev/null || true
helm repo update

helm upgrade --install datadog datadog/datadog \
  -f datadog-values.yaml \
  -n datadog \
  --wait --timeout 5m

echo "Helm release deployed. Waiting for Cluster Agent..."
kubectl wait \
  --for=condition=available \
  deployment/datadog-cluster-agent \
  -n datadog \
  --timeout=120s

echo ""
echo "MutatingWebhookConfiguration status:"
kubectl get mutatingwebhookconfigurations | grep datadog || {
  echo "WARNING: datadog-webhook not found — Admission Controller may not be ready."
  echo "         Proceeding, but DD_* vars may not be injected into pods."
}

echo ""
echo "Waiting 30 seconds for webhook to fully register with the API server..."
sleep 30
echo "Webhook registration wait complete."

# ---------------------------------------------------------------------------
# Step 4: Deploy infrastructure
# ---------------------------------------------------------------------------

log "=== Step 4: Deploy Infrastructure ==="

kubectl apply -f k8s/postgres.yaml
kubectl apply -f k8s/redis.yaml

echo "Waiting for PostgreSQL..."
kubectl wait \
  --for=condition=available \
  deployment/postgres \
  -n insurance-app \
  --timeout=120s

echo "Waiting for Redis..."
kubectl wait \
  --for=condition=available \
  deployment/redis \
  -n insurance-app \
  --timeout=120s

echo "Infrastructure ready."

# ---------------------------------------------------------------------------
# Step 5: Deploy application services
# ---------------------------------------------------------------------------

log "=== Step 5: Deploy Application Services ==="

kubectl apply -f k8s/policy-service.yaml
echo "  policy-service: applied"

kubectl apply -f k8s/claims-service.yaml
echo "  claims-service: applied"

kubectl apply -f k8s/customer-service.yaml
echo "  customer-service: applied"

kubectl apply -f k8s/premium-calculator-service.yaml
echo "  premium-calculator-service: applied"

kubectl apply -f k8s/notification-service.yaml
echo "  notification-service: applied"

kubectl apply -f k8s/api-gateway.yaml
echo "  api-gateway: applied"

kubectl apply -f k8s/web-app.yaml
echo "  web-app: applied"

kubectl apply -f k8s/hpa.yaml
echo "  hpa: applied"

echo ""
echo "Waiting for all deployments to roll out..."
kubectl rollout status deployment --all -n insurance-app --timeout=5m

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

log "=== Deployment Complete ==="

echo ""
echo "LoadBalancer external IPs (may take ~60s to assign):"
kubectl get svc -n insurance-app -o wide | grep LoadBalancer || echo "  (no LoadBalancer services found — check k8s/api-gateway.yaml)"

echo ""
echo "Quick validation commands:"
echo ""
echo "  # Check DD_* injection"
echo "  kubectl exec -n insurance-app deployment/policy-service -- env | sort | grep DD_"
echo ""
echo "  # Get API gateway IP"
echo "  GW=\$(kubectl get svc api-gateway-service -n insurance-app -o jsonpath='{.status.loadBalancer.ingress[0].ip}')"
echo "  curl -sf http://\$GW/health | jq ."
echo ""
echo "  # Run k6 load test"
echo "  kubectl apply -f k8s/k6-job.yaml"
echo "  kubectl wait --for=condition=complete job/k6-load-test -n insurance-app --timeout=15m"
echo "  kubectl logs -n insurance-app job/k6-load-test"
