# Insurance Microservices — Deployment Guide

This guide covers the full deployment lifecycle: building multi-arch Docker images, deploying
the Datadog Agent with the Admission Controller, and validating the end-to-end observability
pipeline on a GKE cluster.

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Orchestrion Compile-Time Instrumentation](#2-orchestrion-compile-time-instrumentation)
3. [Datadog Admission Controller Flow](#3-datadog-admission-controller-flow)
4. [Multi-Arch Docker Builds](#4-multi-arch-docker-builds)
5. [Step-by-Step Deploy Instructions](#5-step-by-step-deploy-instructions)
6. [Validation Steps with Expected Output](#6-validation-steps-with-expected-output)
7. [Troubleshooting](#7-troubleshooting)
8. [Security Notes](#8-security-notes)
9. [Cleanup](#9-cleanup)

---

## 1. Architecture Overview

```
                        ┌─────────────────────────────────────────────────────────────────┐
                        │                    insurance-app namespace                        │
                        │                                                                   │
  Browser ──────────►  │  ┌──────────┐    ┌───────────────┐   ┌─────────────────┐         │
                        │  │  web-app  │    │  api-gateway  │──►│ policy-service  │──► PG  │
                        │  │  :80     │    │  :8080        │   │ :8081           │         │
                        │  └──────────┘    │  (JWT stub,   │   └─────────────────┘         │
                        │                  │  routing)     │   ┌─────────────────┐         │
                        │                  │               │──►│ claims-service  │──► PG   │
                        │                  │               │   │ :8082           │──► Redis│
                        │                  │               │   └─────────────────┘         │
                        │                  │               │   ┌─────────────────┐         │
                        │                  │               │──►│customer-service │──► PG   │
                        │                  │               │   │ :8083           │         │
                        │                  │               │   └─────────────────┘         │
                        │                  │               │   ┌─────────────────┐         │
                        │                  │               │──►│premium-calc-svc │──► Redis│
                        │                  │               │   │ :8084 (stdlib)  │         │
                        │                  │               │   └─────────────────┘         │
                        │                  │               │   ┌─────────────────┐         │
                        │                  └───────────────┘──►│notif-service    │──► Redis│
                        │                                       │ :8085           │         │
                        │                                       └─────────────────┘         │
                        │                                                                   │
                        │  ┌─────────────────────────────────────────────────────────┐    │
                        │  │              Datadog DaemonSet Agent (port 8126)         │    │
                        │  │    ◄── APM traces (TCP) from all services via hostIP     │    │
                        │  └─────────────────────────────────────────────────────────┘    │
                        └─────────────────────────────────────────────────────────────────┘
```

### Service Summary

| Service | Port | Dependencies | Notes |
|---|---|---|---|
| web-app | 80 | api-gateway | Static frontend served by nginx |
| api-gateway | 8080 | All services | JWT stub, request routing |
| policy-service | 8081 | PostgreSQL | Policy CRUD, Gin framework |
| claims-service | 8082 | PostgreSQL, Redis | Claims CRUD, Redis caching |
| customer-service | 8083 | PostgreSQL | Customer profile management |
| premium-calculator-service | 8084 | Redis | Risk calc, net/http stdlib |
| notification-service | 8085 | Redis | Event-driven notifications |

---

## 2. Orchestrion Compile-Time Instrumentation

### How Orchestrion Works

Orchestrion is a **Go toolchain proxy** that intercepts the standard `go build` pipeline using
the `-toolexec` flag. The key insight is that it operates entirely at **compile time**, not at
runtime — meaning there is no performance overhead from wrapping or reflection.

**Build pipeline with Orchestrion:**

```
  go build ./...
       │
       ▼
  orchestrion (toolexec proxy)
       │
       ├── Parses each .go file's AST
       ├── Detects supported library calls (net/http, database/sql, gin, redis, grpc, ...)
       ├── Rewrites the AST to inject tracing spans
       └── Passes modified source to the real compiler (compile, link)
       │
       ▼
  Final binary — tracing built in, no runtime reflection
```

**Initial setup (run once per service):**

```bash
# Pin Orchestrion as a tool dependency — creates orchestrion.tool.go
orchestrion pin

# Verify the pin file was created
ls -la orchestrion.tool.go
```

The `orchestrion.tool.go` file pins the Orchestrion version in `go.mod` / `go.sum` so builds
are reproducible across environments.

**Building with Orchestrion:**

```bash
# Instead of: go build ./...
orchestrion go build ./...

# Inside a Dockerfile:
RUN orchestrion go build -o /app/server ./cmd/server
```

### Auto-Instrumented Libraries

The following libraries are automatically instrumented by Orchestrion with **zero code changes**:

| Library | What Gets Traced |
|---|---|
| `net/http` stdlib server | All HTTP handler registrations and inbound requests — span per request including method, URL, status code |
| `net/http` stdlib client | All outbound `http.Client` requests — span per call with target host and HTTP status |
| `github.com/gin-gonic/gin` | All Gin routes and the full middleware chain — span per matched route |
| `database/sql` stdlib | All SQL queries: `db.Query`, `db.Exec`, `db.QueryContext`, `db.Begin`, `tx.Commit` — span includes sanitized query text |
| `github.com/redis/go-redis/v9` | All Redis commands (GET, SET, LPUSH, EXPIRE, DEL, etc.) — span per command |
| `github.com/go-redis/redis/v8` | All Redis commands (older v8 API) |
| `google.golang.org/grpc` | gRPC server and client interceptors — span per RPC call with service/method names |

### When to Use `//dd:span` Annotations

Orchestrion covers all library-level operations automatically. You add `//dd:span` only for
**your own business logic** that you want to appear as named spans in Datadog APM traces:

| Situation | Action |
|---|---|
| Custom business logic (e.g., `calculateAutoRisk`) | Add `//dd:span` above function |
| Validation functions with complex branching logic | Add `//dd:span` above function |
| Functions that orchestrate multiple traced operations | Add `//dd:span` to create a parent span |
| Any function important enough to appear individually in traces | Add `//dd:span` |

**Example:**

```go
//dd:span policy.type:auto component:risk-engine
func calculateAutoRisk(ctx context.Context, req AutoRiskRequest) (RiskScore, error) {
    // Orchestrion wraps this entire function body in a span named "calculateAutoRisk"
    // with tags policy.type=auto and component=risk-engine
    score := baseScore(req.Age)
    score += vehicleRiskFactor(req.VehicleYear, req.VehicleType)
    score += drivingRecordFactor(req.DrivingRecord)
    return score, nil
}
```

**Requirements for `//dd:span`:**

- The function must accept `context.Context` (or `*http.Request` or `*gin.Context`) as its
  **first parameter** — Orchestrion uses this to propagate the trace context.
- The function must be a **package-level named function**, not an anonymous function or method
  defined inline.
- The annotation comment must appear on the line **immediately** before the `func` keyword with
  no blank lines between them.

### Why NOT to Add Manual Tracing Wrappers

When using Orchestrion, the following patterns are **redundant and harmful**:

| Pattern | Problem |
|---|---|
| `gintrace.New()` middleware | Orchestrion already injects Gin tracing at compile time — adding this creates **duplicate spans** |
| `sqltrace.Register("postgres", ...)` | Orchestrion already instruments `database/sql` — duplicate spans |
| `redistrace.NewClient(...)` | Orchestrion already instruments `go-redis` — duplicate spans |
| Manual `tracer.Start(...)` call | Orchestrion injects `tracer.Start()` automatically using DD_* env vars from the Admission Controller — calling it again causes a second, conflicting tracer instance |

**Correct pattern:** write plain, idiomatic Go code. Orchestrion adds tracing at build time.
The only Datadog import you need is `//dd:span` annotations on your own business functions.

---

## 3. Datadog Admission Controller Flow

The Datadog Admission Controller is a Kubernetes **MutatingAdmissionWebhook** that intercepts
pod creation requests and injects Datadog configuration as environment variables. This means
your application pods never need hardcoded `DD_AGENT_HOST` or tracer bootstrap code.

### Enabling the Webhook: Namespace Label

```yaml
# k8s/namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: insurance-app
  labels:
    admission.datadoghq.com/enabled: "true"  # Tells the webhook to watch this namespace
```

### Opting Pods into Mutation: Pod Annotation

```yaml
# In every Deployment's spec.template.metadata (NOT Deployment.metadata)
spec:
  template:
    metadata:
      annotations:
        admission.datadoghq.com/enabled: "true"  # Opts this pod into mutation
```

> **Critical:** The annotation must be on `.spec.template.metadata.annotations`, which is the
> pod template. If you accidentally place it on `Deployment.metadata.annotations`, the webhook
> never sees it and injection silently does not happen.

### Labels: Why Both Deployment and Pod Template Levels

Labels appear at two levels in every Deployment manifest, and each level serves a different
consumer:

```
Deployment.metadata.labels:
  tags.datadoghq.com/env: demo2        ← Datadog Agent reads these via kube-state-metrics
  tags.datadoghq.com/service: svc      ← Used for infrastructure metric correlation (CPU, memory)
  tags.datadoghq.com/version: "1.0.0"  ← Correlates deployment version with infrastructure metrics

spec.template.metadata.labels:
  tags.datadoghq.com/env: demo2        ← Admission Controller reads these at pod creation time
  tags.datadoghq.com/service: svc      ← → injects DD_SERVICE=svc into the container
  tags.datadoghq.com/version: "1.0.0"  ← → injects DD_VERSION=1.0.0 into the container
```

Both levels must be present. Omitting the pod template labels means the Admission Controller
has nothing to read and will not inject `DD_ENV`, `DD_SERVICE`, or `DD_VERSION`.

### What the Admission Controller Injects Automatically

When a pod matching the criteria is created, the Admission Controller mutates the pod spec and
injects these environment variables into every container:

```
DD_AGENT_HOST=<node-ip>                    (from hostip configMode — resolves to the node's IP)
DD_TRACE_AGENT_URL=http://<node-ip>:8126   (TCP APM endpoint on the DaemonSet agent)
DD_ENTITY_ID=<pod-uid>                     (enables entity-level tagging in Datadog)
DD_EXTERNAL_ENV=demo2                      (env propagation for cross-service correlation)
DD_ENV=demo2                               (from tags.datadoghq.com/env label)
DD_SERVICE=policy-service                  (from tags.datadoghq.com/service label)
DD_VERSION=1.0.0                           (from tags.datadoghq.com/version label)
```

### The Only DD_* Variable You Set Manually

```yaml
env:
  - name: DD_PROFILING_ENABLED
    value: "true"
```

`DD_PROFILING_ENABLED=true` enables the **continuous profiler**. Orchestrion injects the
profiler initialization code at build time, but the profiler only activates when this env var
is `true` at runtime. This gives you on/off control without rebuilding. All other `DD_*`
variables come from the Admission Controller.

### Injection Flow Diagram

```
  kubectl apply -f k8s/policy-service.yaml
          │
          ▼
  Kubernetes API Server receives pod creation request
          │
          ▼  (MutatingAdmissionWebhook intercepts)
  Datadog Cluster Agent (Admission Controller)
          │
          ├── Reads pod labels: tags.datadoghq.com/{env,service,version}
          ├── Resolves node IP (hostip mode)
          └── Injects DD_* env vars into pod spec
          │
          ▼
  Pod starts with DD_AGENT_HOST, DD_SERVICE, DD_ENV, etc. already set
          │
          ▼
  Orchestrion-compiled binary reads DD_* vars → tracer auto-starts → spans flow to agent
```

---

## 4. Multi-Arch Docker Builds

### The Problem: Mac M4 vs GKE Nodes

- **Development machine:** Mac M4 (Apple Silicon, `arm64` architecture)
- **GKE cluster nodes:** `linux/amd64` architecture

When you run `docker build` on an M4 without specifying `--platform`, Docker produces an
`arm64` image. When Kubernetes tries to schedule that image on an `amd64` node, the container
runtime cannot execute the binary and the pod fails with:

```
exec /app/server: exec format error
```

### The Solution: Docker Buildx with QEMU

`docker buildx` extends the standard Docker build with multi-platform support. It uses QEMU
user-mode emulation (or cross-compilation where available) to produce fat manifests containing
images for multiple architectures. A single image tag then serves both `arm64` (your laptop)
and `amd64` (GKE nodes).

### One-Time Builder Setup

```bash
# Create a buildx builder that supports multi-arch (only needed once per machine)
docker buildx create --use --name multiarch-builder

# Bootstrap the builder and verify QEMU platforms are available
docker buildx inspect --bootstrap
# Look for: linux/amd64, linux/arm64 in the "Platforms:" list
```

### Build and Push All Services

```bash
# Build all Go microservices
for svc in policy-service claims-service customer-service premium-calculator-service notification-service api-gateway; do
  docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --tag wwongpai/insurance-${svc}:1.0.0 \
    --push \
    ./services/${svc}
done

# Build the web frontend
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --tag wwongpai/insurance-web-app:1.0.0 \
  --push \
  ./services/web-app
```

> **Note:** `--push` is required with `buildx` multi-platform builds because the local Docker
> daemon image store cannot hold multi-arch manifests. The image is pushed directly to the
> registry and pulled by GKE nodes as needed.

### Verifying the Multi-Arch Manifest

```bash
docker buildx imagetools inspect wwongpai/insurance-policy-service:1.0.0
```

Expected output structure:

```
Name:      docker.io/wwongpai/insurance-policy-service:1.0.0
MediaType: application/vnd.docker.distribution.manifest.list.v2+json
Digest:    sha256:<fat-manifest-digest>

Manifests:
  Name:      docker.io/wwongpai/insurance-policy-service:1.0.0@sha256:<amd64-digest>
  MediaType: application/vnd.docker.distribution.manifest.v2+json
  Platform:  linux/amd64

  Name:      docker.io/wwongpai/insurance-policy-service:1.0.0@sha256:<arm64-digest>
  MediaType: application/vnd.docker.distribution.manifest.v2+json
  Platform:  linux/arm64
```

Both `linux/amd64` and `linux/arm64` must appear. If only one platform is shown, the build
did not use `--platform linux/amd64,linux/arm64`.

---

## 5. Step-by-Step Deploy Instructions

### Step 1: Prerequisites

```bash
# Verify you are targeting the correct cluster
kubectl config use-context warach-gke-go-app
kubectl get nodes
# Expected: nodes with status Ready, instance type e2-standard-* or similar

# Verify Helm is installed and at v3+
helm version
# Expected: version.BuildInfo{Version:"v3.x.x", ...}

# Verify Docker buildx is available
docker buildx ls
# Expected: multiarch-builder or default listed with linux/amd64, linux/arm64 support
```

### Step 2: Create Namespaces and Secrets

```bash
# Create the insurance-app namespace (with Admission Controller label)
kubectl apply -f k8s/namespace.yaml

# --- insurance-app namespace secrets ---

# Datadog API and APP keys
kubectl create secret generic datadog-secret \
  --from-literal=api-key=<YOUR_DD_API_KEY> \
  --from-literal=app-key=<YOUR_DD_APP_KEY> \
  -n insurance-app

# PostgreSQL password (used by policy, claims, and customer services)
kubectl create secret generic postgres-secret \
  --from-literal=password=<STRONG_PASSWORD_HERE> \
  -n insurance-app

# Redis password (used by claims, premium-calculator, and notification services)
kubectl create secret generic redis-secret \
  --from-literal=password=<STRONG_PASSWORD_HERE> \
  -n insurance-app

# --- datadog namespace ---

kubectl create namespace datadog

kubectl create secret generic datadog-secret \
  --from-literal=api-key=<YOUR_DD_API_KEY> \
  --from-literal=app-key=<YOUR_DD_APP_KEY> \
  -n datadog
```

> Replace `<YOUR_DD_API_KEY>`, `<YOUR_DD_APP_KEY>`, and the password placeholders with real
> values. Never commit these values to source control.

### Step 3: Build and Push Multi-Arch Images

```bash
# Create builder (idempotent — reuses existing builder if already created)
docker buildx create --use --name multiarch-builder 2>/dev/null || docker buildx use multiarch-builder
docker buildx inspect --bootstrap

# Build all Go microservices
for svc in policy-service claims-service customer-service premium-calculator-service notification-service api-gateway; do
  echo "Building ${svc}..."
  docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --tag wwongpai/insurance-${svc}:1.0.0 \
    --push \
    ./services/${svc}
done

# Build web frontend
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --tag wwongpai/insurance-web-app:1.0.0 \
  --push \
  ./services/web-app
```

Alternatively, run `./build.sh` which wraps these commands with error handling.

### Step 4: Deploy Datadog Agent (BEFORE App Pods)

> **Critical ordering requirement:** The Datadog Cluster Agent and its MutatingAdmissionWebhook
> must be registered with the Kubernetes API server **before** any application pods are created.
> If application pods start before the webhook exists, the API server cannot call the webhook
> and DD_* env vars will not be injected. You would need to restart all app pods after the
> fact to get injection.

```bash
# Add the Datadog Helm repository
helm repo add datadog https://helm.datadoghq.com
helm repo update

# Deploy the Datadog Agent (DaemonSet + Cluster Agent + Admission Controller)
helm install datadog datadog/datadog \
  -f datadog-values.yaml \
  -n datadog \
  --wait --timeout 5m

# Verify Cluster Agent deployment is available
kubectl wait --for=condition=available deployment/datadog-cluster-agent \
  -n datadog --timeout=120s

# Verify the MutatingWebhookConfiguration was registered
kubectl get mutatingwebhookconfigurations | grep datadog
# Must show: datadog-webhook

# Wait for webhook to fully register with the API server
# (the deployment becoming available and the webhook being callable are slightly different events)
sleep 30
```

### Step 5: Deploy Infrastructure

```bash
# Deploy PostgreSQL
kubectl apply -f k8s/postgres.yaml

# Deploy Redis
kubectl apply -f k8s/redis.yaml

# Wait for both to be ready before deploying services
kubectl wait --for=condition=available deployment/postgres \
  -n insurance-app --timeout=120s
kubectl wait --for=condition=available deployment/redis \
  -n insurance-app --timeout=120s
```

### Step 6: Deploy Application Services

```bash
# Deploy all microservices
kubectl apply -f k8s/policy-service.yaml
kubectl apply -f k8s/claims-service.yaml
kubectl apply -f k8s/customer-service.yaml
kubectl apply -f k8s/premium-calculator-service.yaml
kubectl apply -f k8s/notification-service.yaml
kubectl apply -f k8s/api-gateway.yaml
kubectl apply -f k8s/web-app.yaml

# Deploy Horizontal Pod Autoscalers
kubectl apply -f k8s/hpa.yaml

# Wait for all deployments to finish rolling out
kubectl rollout status deployment --all -n insurance-app --timeout=5m
```

### Step 7: Run k6 Load Test

The k6 load test generates realistic traffic so APM traces and metrics populate in Datadog.

```bash
# Submit the k6 job
kubectl apply -f k8s/k6-job.yaml

# Wait for the job to complete (up to 15 minutes for full test run)
kubectl wait --for=condition=complete job/k6-load-test \
  -n insurance-app --timeout=15m

# View the test summary output
kubectl logs -n insurance-app job/k6-load-test
```

---

## 6. Validation Steps with Expected Output

### 6.1 Multi-Arch Manifest Verification

```bash
docker buildx imagetools inspect wwongpai/insurance-policy-service:1.0.0
```

**Expected:** Output shows a manifest list with **both** `linux/amd64` and `linux/arm64`
entries, each with a unique digest hash. If only one platform appears, the image was not
built with `--platform linux/amd64,linux/arm64`.

### 6.2 Admission Controller Injection Verification

```bash
kubectl exec -n insurance-app deployment/policy-service -- env | sort | grep DD_
```

**Expected — all of the following must appear:**

```
DD_AGENT_HOST=<node-ip>
DD_ENTITY_ID=<pod-uid>
DD_ENV=demo2
DD_EXTERNAL_ENV=demo2
DD_PROFILING_ENABLED=true
DD_SERVICE=policy-service
DD_TRACE_AGENT_URL=http://<node-ip>:8126
DD_VERSION=1.0.0
```

If `DD_AGENT_HOST` and `DD_SERVICE` are missing, the Admission Controller did not inject.
See Troubleshooting section 7.1.

### 6.3 APM Trace Verification

```bash
kubectl logs -n datadog -l app=datadog-agent -c agent --tail=100 | grep -i "apm\|trace"
```

**Expected:** Log lines containing `"APM server started"` and trace intake messages showing
incoming spans. If you see `connection refused` errors in app pod logs when connecting to
`DD_TRACE_AGENT_URL`, the DaemonSet agent may not be running on that node.

### 6.4 API Smoke Test

```bash
# Get the external IP of the api-gateway LoadBalancer service
GW=$(kubectl get svc api-gateway-service -n insurance-app \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
echo "Gateway IP: $GW"

# Health check — should return {"status":"ok"} or similar
curl -sf http://$GW/health | jq .

# Create a test customer
curl -sf -X POST http://$GW/api/v1/customers \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -d '{
    "firstName": "Test",
    "lastName": "User",
    "email": "test.smoke@example.invalid",
    "phone": "+15551234567",
    "dateOfBirth": "1990-01-01"
  }' | jq .

# Calculate auto insurance premium
curl -sf -X POST http://$GW/api/v1/calculate/auto \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -d '{
    "age": 35,
    "vehicleYear": 2022,
    "vehicleType": "sedan",
    "drivingRecord": "clean"
  }' | jq .

# Get claims statistics
curl -sf http://$GW/api/v1/claims/stats \
  -H 'Authorization: Bearer demo-token' | jq .
```

**Expected:** All four commands return JSON responses with HTTP 200. Any non-2xx response or
connection refused indicates a service or routing problem.

### 6.5 Datadog UI Checks (Manual Verification)

After the k6 load test completes, verify the following in the Datadog web UI:

**APM → Services**
- All 6 services are visible: `api-gateway`, `policy-service`, `claims-service`,
  `customer-service`, `premium-calculator-service`, `notification-service`
- All services are tagged with `env:demo2`
- Each service shows request rate, error rate, and p99 latency

**Infrastructure → Kubernetes**
- Cluster `warach-gke-go-app` is visible
- Both `insurance-app` and `datadog` namespaces are listed
- Pod status, CPU, and memory graphs are populated

**Logs**
- Structured JSON log entries from all services
- Each log entry contains `dd.trace_id` and `dd.span_id` fields for trace-log correlation
- Logs are filterable by `service:policy-service`, `env:demo2`, etc.

**APM → Service Map**
- Service dependency graph is visible showing connections:
  - `api-gateway` → `policy-service`, `claims-service`, `customer-service`,
    `premium-calculator-service`, `notification-service`
  - `claims-service` → `redis`
  - `policy-service` → `postgres`

**APM → Traces**
- Trace list populated with spans from all services
- Each trace shows correct service names and `env:demo2` tag
- Flame graph for individual traces shows database and Redis sub-spans

### 6.6 Security Scan

```bash
# Scan all images for HIGH and CRITICAL CVEs
for svc in policy-service claims-service customer-service \
           premium-calculator-service notification-service \
           api-gateway web-app; do
  echo "=== Scanning wwongpai/insurance-${svc}:1.0.0 ==="
  trivy image --severity HIGH,CRITICAL wwongpai/insurance-${svc}:1.0.0
done

# Verify no hardcoded secrets in source files
grep -rn "api.key\|password\|DD_API_KEY\|api_key" \
  --include="*.go" --include="*.yaml" --include="*.json" --include="Dockerfile" \
  services/ k8s/ && echo "WARNING: Potential secrets found" || echo "CLEAN: No hardcoded secrets"
```

**Expected for grep check:** `CLEAN: No hardcoded secrets` — all credentials are stored in
Kubernetes Secrets and referenced by name, never as literal values in source files.

---

## 7. Troubleshooting

### 7.1 Admission Controller Not Injecting DD_* Vars

**Symptom:** `kubectl exec deployment/policy-service -- env | grep DD_` shows no `DD_AGENT_HOST`

Work through these causes in order:

**1. Webhook not registered yet**

```bash
kubectl get mutatingwebhookconfigurations | grep datadog
# If missing: the cluster agent is not ready yet
kubectl get pods -n datadog
kubectl describe deployment datadog-cluster-agent -n datadog
```

Wait for the cluster agent to be fully running, then restart your app pods.

**2. Wrong annotation placement**

```bash
kubectl get deployment policy-service -n insurance-app -o yaml | grep -A5 "annotations:"
```

The annotation `admission.datadoghq.com/enabled: "true"` must appear under
`.spec.template.metadata.annotations`, not under the top-level `metadata.annotations`.

**3. Namespace missing the label**

```bash
kubectl get ns insurance-app --show-labels
# Must contain: admission.datadoghq.com/enabled=true
# If missing:
kubectl label namespace insurance-app admission.datadoghq.com/enabled=true
```

**4. Pod predates the webhook**

If pods were created before the webhook was registered, they will not be mutated retroactively.

```bash
# Force a rolling restart so new pods are created after the webhook is live
kubectl rollout restart deployment/policy-service -n insurance-app
kubectl rollout restart deployment/claims-service -n insurance-app
# ... repeat for all deployments
```

### 7.2 Traces Not Appearing in Datadog

Check in this order:

```bash
# 1. Confirm DD_TRACE_AGENT_URL is injected
kubectl exec deployment/policy-service -n insurance-app -- env | grep DD_TRACE_AGENT_URL

# 2. Confirm the DaemonSet agent is running on the same node as the pod
kubectl get pods -n datadog -o wide          # shows which node each agent pod runs on
kubectl get pods -n insurance-app -o wide    # shows which node each app pod runs on

# 3. Check agent logs for APM intake activity
kubectl logs -n datadog -l app=datadog-agent -c agent | grep -i "apm\|trace"

# 4. Verify orchestrion.tool.go exists in each service (required for Orchestrion builds)
ls services/*/orchestrion.tool.go

# 5. Confirm the service is actually handling requests (no traces without requests)
kubectl logs -n insurance-app deployment/policy-service --tail=50
```

### 7.3 Wrong Architecture Error

**Symptom:** Pod fails immediately with `exec /app/server: exec format error` or
`standard_init_linux.go:228: exec user process caused: exec format error`

**Diagnosis:**

```bash
kubectl describe pod -n insurance-app -l app=policy-service | grep -A10 "Events:"
# Look for: "exec format error"

# Inspect the image manifest to see what architectures were built
docker buildx imagetools inspect wwongpai/insurance-policy-service:1.0.0 | grep -A2 "Platform"
```

**Fix:** The image was built for `arm64` only (built on M4 without `--platform`). Rebuild:

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --tag wwongpai/insurance-policy-service:1.0.0 \
  --push \
  ./services/policy-service

# Force pods to pull the new multi-arch image
kubectl rollout restart deployment/policy-service -n insurance-app
```

### 7.4 PostgreSQL Connection Failures

**Symptom:** Service logs show `pq: password authentication failed` or `connection refused`

```bash
# Check what POSTGRES_* vars the service sees
kubectl exec -n insurance-app deployment/policy-service -- env | grep POSTGRES

# Check PostgreSQL pod logs
kubectl logs -n insurance-app deployment/postgres --tail=50

# Verify PostgreSQL is accepting connections
kubectl exec -n insurance-app deployment/postgres -- \
  pg_isready -U insuranceuser -d insurancedb
# Expected: /var/run/postgresql:5432 - accepting connections

# Check the postgres-secret exists and has the right key
kubectl get secret postgres-secret -n insurance-app -o jsonpath='{.data}' | python3 -m json.tool
```

If the secret password does not match what PostgreSQL was initialized with, delete and recreate
the PostgreSQL deployment after updating the secret.

### 7.5 Redis Connection Failures

**Symptom:** Service logs show `WRONGPASS invalid username-password pair` or `dial tcp: refused`

```bash
# Check what REDIS_* vars the service sees
kubectl exec -n insurance-app deployment/claims-service -- env | grep REDIS

# Check Redis pod logs
kubectl logs -n insurance-app deployment/redis --tail=50

# Test Redis connectivity and authentication
kubectl exec -n insurance-app deployment/redis -- \
  redis-cli -a "${REDIS_PASSWORD}" ping
# Expected: PONG

# Check the redis-secret exists
kubectl get secret redis-secret -n insurance-app -o jsonpath='{.data.password}' | base64 -d
```

### 7.6 Helm Upgrade Conflicts

If `helm install` fails because a release already exists:

```bash
# Use upgrade --install to be idempotent
helm upgrade --install datadog datadog/datadog \
  -f datadog-values.yaml \
  -n datadog \
  --wait --timeout 5m
```

---

## 8. Security Notes

The following measures prevent credential leaks in this project:

1. **All secrets are Kubernetes Secrets mounted as env vars.** Passwords and API keys are never
   written to manifest files, Dockerfiles, or application source code. They exist only in
   etcd (encrypted at rest on GKE) and in the running pod's memory.

2. **`secrets-template.txt` is intentionally a `.txt` file**, not `.yaml`. Running
   `kubectl apply -f k8s/` applies all `.yaml` files in the directory automatically. A `.txt`
   extension ensures the template file is never accidentally applied to the cluster.

3. **Multi-stage Docker builds** ensure that the Go toolchain, source code, Orchestrion binary,
   and intermediate build artifacts do not appear in the final runtime image. The runtime stage
   copies only the compiled binary.

4. **Runtime base image is `debian:bookworm-slim`** — a minimal image with no development
   tools, package managers exposed to external networks, or unnecessary OS services.

5. **Non-root user (uid 1001)** runs all service processes in containers. This limits the blast
   radius if a container is compromised — the process cannot write to `/etc`, install packages,
   or access other users' files.

6. **`DD_API_KEY` and `DD_APP_KEY` never appear in any source file.** The Helm chart and
   Kubernetes manifests reference them as `secretKeyRef` lookups against the `datadog-secret`
   Kubernetes Secret. The secret is created imperatively at deploy time from environment
   variables, not from a file checked into git.

7. **Synthetic test data** in the k6 load test uses email addresses ending in
   `@example.invalid` — a domain that is guaranteed to be unresolvable per RFC 2606. No real
   PII is generated or stored during load testing.

---

## 9. Cleanup

Remove all resources when the demo is complete:

```bash
# Remove the entire application namespace (deletes all deployments, services, secrets, PVCs)
kubectl delete namespace insurance-app

# Uninstall the Datadog Helm release and remove its namespace
helm uninstall datadog -n datadog
kubectl delete namespace datadog

# Remove the MutatingWebhookConfiguration left behind by Datadog (if any)
kubectl delete mutatingwebhookconfigurations datadog-webhook 2>/dev/null || true

# Remove the multi-arch buildx builder (optional — can be reused for future projects)
docker buildx rm multiarch-builder

# Remove local Docker image references (optional — frees disk space)
for svc in policy-service claims-service customer-service \
           premium-calculator-service notification-service \
           api-gateway web-app; do
  docker rmi wwongpai/insurance-${svc}:1.0.0 2>/dev/null || true
done

echo "Cleanup complete."
```

> After deleting the `insurance-app` namespace, GKE will release the LoadBalancer external IPs
> assigned to `api-gateway-service` and `web-app-service`. This may take a few minutes to
> propagate through GCP's load balancer backend.
