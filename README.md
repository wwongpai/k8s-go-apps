# k8s-go-apps

Go microservices on GKE with **zero-code Datadog APM** via [Orchestrion](https://datadoghq.dev/orchestrion/) compile-time instrumentation.

> **What makes this different:** No `tracer.Start()`, no middleware wrappers, no `sqltrace.Register()`. Every HTTP, SQL, Redis, and gRPC span is injected **at compile time** by Orchestrion — the application source code stays plain, idiomatic Go.

---

## Table of Contents

1. [Architecture](#1-architecture)
2. [How Orchestrion Works](#2-how-orchestrion-works)
3. [Migrating a Go App to Orchestrion](#3-migrating-a-go-app-to-orchestrion)
4. [Datadog on Kubernetes — The Full Picture](#4-datadog-on-kubernetes--the-full-picture)
5. [Step-by-Step Setup](#5-step-by-step-setup)
6. [Critical APM Gotchas](#6-critical-apm-gotchas)
7. [Validation](#7-validation)
8. [Troubleshooting](#8-troubleshooting)
9. [Cleanup](#9-cleanup)

---

## 1. Architecture

```
  Browser
     │
     ▼
┌──────────────────────────────────────────────── insurance-app namespace ─┐
│                                                                            │
│  ┌─────────┐    ┌─────────────┐    ┌──────────────────┐                  │
│  │ web-app │───►│ api-gateway │───►│ policy-service   │──► PostgreSQL    │
│  │ :80     │    │ :8080       │    │ :8081  (Gin)     │                  │
│  └─────────┘    │ (Gin, JWT)  │    └──────────────────┘                  │
│                 │             │    ┌──────────────────┐                  │
│                 │             │───►│ claims-service   │──► PostgreSQL    │
│                 │             │    │ :8082  (Gin)     │──► Redis         │
│                 │             │    └──────────────────┘                  │
│                 │             │    ┌──────────────────┐                  │
│                 │             │───►│ customer-service │──► PostgreSQL    │
│                 │             │    │ :8083  (Gin)     │                  │
│                 │             │    └──────────────────┘                  │
│                 │             │    ┌──────────────────┐                  │
│                 │             │───►│ premium-calc-svc │──► Redis         │
│                 │             │    │ :8084  (stdlib)  │                  │
│                 │             │    └──────────────────┘                  │
│                 │             │    ┌──────────────────┐                  │
│                 └─────────────┘───►│ notif-service    │──► Redis         │
│                                    │ :8085  (stdlib)  │                  │
│                                    └──────────────────┘                  │
│                                                                            │
│  ┌──────────────────────────────────────────────────────────────────┐    │
│  │  Datadog DaemonSet Agent (port 8126) — one pod per node          │    │
│  │  APM traces (TCP) ◄── all services via DD_AGENT_HOST=<nodeIP>    │    │
│  └──────────────────────────────────────────────────────────────────┘    │
└────────────────────────────────────────────────────────────────────────────┘
```

| Service | Port | Framework | Dependencies |
|---|---|---|---|
| web-app | 80 | nginx | api-gateway |
| api-gateway | 8080 | Gin | All downstream services |
| policy-service | 8081 | Gin | PostgreSQL |
| claims-service | 8082 | Gin | PostgreSQL, Redis |
| customer-service | 8083 | Gin | PostgreSQL |
| premium-calculator-service | 8084 | net/http stdlib | Redis |
| notification-service | 8085 | net/http stdlib | Redis |
| orchestrion-demo | 8090 | net/http stdlib | _(none)_ |

---

## 2. How Orchestrion Works

### The Core Idea

Orchestrion is a **Go toolchain proxy**. When you run `orchestrion go build`, it intercepts
every file the Go compiler processes, parses its AST (Abstract Syntax Tree), detects calls to
supported libraries, rewrites those calls to include tracing spans, and hands the modified code
to the real compiler.

The result: a fully instrumented binary with **zero changes to your source code**.

```
  Your Source Code (plain Go)
          │
          ▼
  orchestrion go build          ← proxy wraps standard go build
          │
          ├─ parse AST of every .go file
          ├─ detect: net/http, gin, database/sql, go-redis, grpc, ...
          ├─ inject: span.Start(), span.Finish(), context propagation
          └─ pass modified AST to real Go compiler
          │
          ▼
  Final Binary — APM built in, no runtime reflection overhead
```

### What Gets Instrumented Automatically

| Library | What Gets Traced |
|---|---|
| `net/http` server | Every `HandleFunc` / `Handle` registration → span per inbound request with method, URL, status code |
| `net/http` client | Every `http.Client.Do` / `Get` / `Post` call → span per outbound request with target host and status |
| `github.com/gin-gonic/gin` | Every route and middleware chain → span per matched route |
| `database/sql` | `Query`, `Exec`, `QueryContext`, `Begin`, `Commit`, `Rollback` → span with sanitized SQL |
| `github.com/redis/go-redis/v9` | Every Redis command (GET, SET, HGET, LPUSH, EXPIRE, DEL, ...) → span per command |
| `github.com/go-redis/redis/v8` | Same as above for the older v8 API |
| `google.golang.org/grpc` | Server and client interceptors → span per RPC call with service/method name |

### Adding Custom Spans with `//dd:span`

Orchestrion covers all library calls automatically. For **your own business logic**, add a
`//dd:span` comment on the line immediately before the function:

```go
//dd:span policy.type:auto component:risk-engine
func calculateAutoRisk(ctx context.Context, req AutoRiskRequest) (RiskScore, error) {
    // Orchestrion wraps this entire function in a named span: "calculateAutoRisk"
    // with tags: policy.type=auto, component=risk-engine
    score := baseScore(req.Age)
    score += vehicleRiskFactor(req.VehicleYear, req.VehicleType)
    score += drivingRecordFactor(req.DrivingRecord)
    return score, nil
}
```

**Requirements for `//dd:span` to work:**

1. The function's **first parameter** must be `context.Context` (or `*http.Request` or `*gin.Context`) — Orchestrion uses this to propagate the trace context.
2. The function must be a **named, package-level function** — not an anonymous func or a method defined inline.
3. The `//dd:span` comment must be on the **line immediately before** `func` with no blank lines between.

### What NOT to Do with Orchestrion

These patterns are **redundant and harmful** when Orchestrion is in use:

| Anti-pattern | Why it breaks things |
|---|---|
| `tracer.Start(tracer.WithService(...))` | Orchestrion injects `tracer.Start()` automatically — calling it again creates a second conflicting tracer instance |
| `gintrace.New()` middleware | Orchestrion already injects Gin tracing at compile time — this creates **duplicate spans** for every request |
| `sqltrace.Register("postgres", ...)` | Orchestrion already instruments `database/sql` — duplicate spans |
| `redistrace.NewClient(...)` | Orchestrion already instruments go-redis — duplicate spans |
| Manual `span.Start()` / `span.Finish()` on HTTP handlers | Already traced by Orchestrion |

**Rule:** Write plain, idiomatic Go. The only Datadog-specific thing you need is `//dd:span`
on your own business functions.

---

## 3. Migrating a Go App to Orchestrion

This is the exact process followed for all 6 insurance microservices in this repo.

### Step 1 — Remove Manual Instrumentation

Delete everything that was manually added for tracing:

```go
// REMOVE these imports:
import (
    "github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
    "github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
    gintrace "github.com/DataDog/dd-trace-go/v2/contrib/gin-gonic/gin"
    redistrace "github.com/DataDog/dd-trace-go/v2/contrib/redis/go-redis.v9"
)

// REMOVE from main():
tracer.Start(
    tracer.WithService("policy-service"),
    tracer.WithEnv("demo2"),
)
defer tracer.Stop()

// REMOVE Gin tracer middleware:
r.Use(gintrace.Middleware("policy-service"))

// REMOVE traced Redis client wrapper:
rdb = redistrace.NewClient(redisOpts)
```

After removal, the app is plain Go with zero Datadog imports.

### Step 2 — Keep `//dd:span` Annotations (optional)

`//dd:span` annotations on business logic functions are safe to keep — they are just comments
that Orchestrion processes at build time. If you have custom span names or tags on key
functions, keep them.

### Step 3 — Update `go.mod` to Go 1.25

Orchestrion v1.8.0+ requires Go 1.25 (due to `dd-trace-go/orchestrion/all/v2@v2.7.0`):

```
module github.com/wwongpai/policy-service

go 1.25   ← was go 1.24, must be updated
```

### Step 4 — Update the Dockerfile

Replace the standard `go build` with Orchestrion:

```dockerfile
FROM golang:1.25 AS builder        # ← Go 1.25 required
WORKDIR /app

# Install Orchestrion CLI
RUN go install github.com/DataDog/orchestrion@latest

# Copy source
COPY go.mod ./
COPY *.go ./

# Pin Orchestrion as a tool dependency (creates orchestrion.tool.go, updates go.mod/go.sum)
RUN orchestrion pin && go mod tidy

# Build — Orchestrion intercepts and injects tracing at compile time
RUN orchestrion go build -o /app/service .

# --- minimal runtime image ---
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/service /service
EXPOSE 8080
ENTRYPOINT ["/service"]
```

The key difference from a standard Dockerfile is line 3 (install orchestrion) and line 10
(`orchestrion go build` instead of `go build`). Everything else stays the same.

### Step 5 — Update the Kubernetes Manifest

Add the Unified Service Tagging labels and the Admission Controller label to the pod template:

```yaml
spec:
  template:
    metadata:
      labels:
        app: policy-service
        # Unified Service Tagging — Admission Controller reads these to inject DD_* env vars
        tags.datadoghq.com/env: "demo2"
        tags.datadoghq.com/service: "policy-service"
        tags.datadoghq.com/version: "1.0.3"
        # IMPORTANT: This must be a LABEL (not an annotation) for the webhook to trigger
        admission.datadoghq.com/enabled: "true"
```

That is everything. The Orchestrion-compiled binary reads `DD_AGENT_HOST` (injected by the
Admission Controller) and auto-starts the tracer on first request.

### Before vs After (diff summary)

```diff
- import "github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
- import gintrace "github.com/DataDog/dd-trace-go/v2/contrib/gin-gonic/gin"
- import redistrace "github.com/DataDog/dd-trace-go/v2/contrib/redis/go-redis.v9"

  func main() {
-     tracer.Start(tracer.WithService("policy-service"), tracer.WithEnv("demo2"))
-     defer tracer.Stop()
      r := gin.New()
-     r.Use(gintrace.Middleware("policy-service"))
      ...
-     rdb = redistrace.NewClient(opts)
+     rdb = redis.NewClient(opts)   // plain go-redis — Orchestrion instruments it at build time
  }
```

Source code shrinks. Observability increases. No tradeoff.

---

## 4. Datadog on Kubernetes — The Full Picture

### Component Overview

```
┌─────────────────────────────────────── datadog namespace ──────────────┐
│                                                                          │
│  ┌─────────────────────────────┐   ┌──────────────────────────────┐    │
│  │  Datadog Cluster Agent      │   │  Datadog DaemonSet Agent     │    │
│  │  (2 replicas for HA)        │   │  (1 pod per node)            │    │
│  │                             │   │                              │    │
│  │  - Admission Controller     │   │  - Receives APM traces :8126 │    │
│  │  - MutatingWebhook          │   │  - Collects container logs   │    │
│  │  - Cluster-level metrics    │   │  - Node-level metrics        │    │
│  │  - kube-state-metrics       │   │  - Process monitoring        │    │
│  └─────────────────────────────┘   └──────────────────────────────┘    │
└──────────────────────────────────────────────────────────────────────────┘
```

### Admission Controller — Automatic DD_* Injection

The Cluster Agent runs a **MutatingAdmissionWebhook** that intercepts pod creation and injects
Datadog configuration as environment variables. Your pods never need hardcoded `DD_AGENT_HOST`.

**How mutation is triggered (two conditions required):**

```yaml
# 1. The NAMESPACE must have this label:
apiVersion: v1
kind: Namespace
metadata:
  name: insurance-app
  labels:
    admission.datadoghq.com/enabled: "true"

# 2. Each POD TEMPLATE must have this LABEL (not annotation!):
spec:
  template:
    metadata:
      labels:
        admission.datadoghq.com/enabled: "true"   # ← LABEL, not annotation
```

**What the Admission Controller injects into every container:**

```
DD_AGENT_HOST          = <node-ip>               (hostip mode — the node running this pod)
DD_TRACE_AGENT_URL     = http://<node-ip>:8126   (TCP APM endpoint on the DaemonSet agent)
DD_ENTITY_ID           = <pod-uid>               (entity-level tagging)
DD_ENV                 = demo2                   (from tags.datadoghq.com/env label)
DD_SERVICE             = policy-service          (from tags.datadoghq.com/service label)
DD_VERSION             = 1.0.3                   (from tags.datadoghq.com/version label)
```

When the Orchestrion-compiled binary starts, it reads these env vars and automatically
initializes the Datadog tracer — no code required.

### Helm Values — Key Configuration (`datadog-values.yaml`)

```yaml
datadog:
  site: datadoghq.com
  clusterName: warach-gke-go-app
  apm:
    portEnabled: true
    port: 8126
  logs:
    enabled: true
    containerCollectAll: true

clusterAgent:
  enabled: true
  replicas: 2
  admissionController:
    enabled: true
    mutateUnlabelled: false   # only mutate pods with the label
    injectConfig:
      enabled: true
    injectTags:
      enabled: true
    configMode: hostip        # injects the node IP as DD_AGENT_HOST

agents:
  containers:
    traceAgent:
      env:
        # CRITICAL FIX: Agent 7.76+ defaults to computing stats from span kinds
        # and dropping the raw span payloads — this disables that behavior
        - name: DD_APM_COMPUTE_STATS_BY_SPAN_KIND
          value: "false"
        - name: DD_APM_PEER_TAGS_AGGREGATION
          value: "false"
```

---

## 5. Step-by-Step Setup

### Prerequisites

```bash
# Verify cluster access
kubectl config use-context warach-gke-go-app
kubectl get nodes

# Verify tools
helm version       # v3+
docker buildx ls   # must list a builder with linux/amd64, linux/arm64
```

### Step 1 — Create Namespace and Secrets

```bash
# Create the insurance-app namespace (includes Admission Controller label)
kubectl apply -f k8s/namespace.yaml

# Datadog API/App keys (insurance-app namespace — referenced by app pods)
kubectl create secret generic datadog-secret \
  --from-literal=api-key=<YOUR_DD_API_KEY> \
  --from-literal=app-key=<YOUR_DD_APP_KEY> \
  -n insurance-app

# PostgreSQL password
kubectl create secret generic postgres-secret \
  --from-literal=password=<STRONG_PASSWORD> \
  -n insurance-app

# Redis password
kubectl create secret generic redis-secret \
  --from-literal=password=<STRONG_PASSWORD> \
  -n insurance-app

# Datadog namespace + secret (used by the Helm chart)
kubectl create namespace datadog
kubectl create secret generic datadog-secret \
  --from-literal=api-key=<YOUR_DD_API_KEY> \
  --from-literal=app-key=<YOUR_DD_APP_KEY> \
  -n datadog
```

### Step 2 — Deploy Datadog Agent (BEFORE application pods)

> The MutatingAdmissionWebhook must be registered **before** any app pods are created.
> If pods start first, the Admission Controller cannot inject DD_* vars into them.

```bash
helm repo add datadog https://helm.datadoghq.com && helm repo update

helm upgrade --install datadog datadog/datadog \
  -f datadog-values.yaml \
  -n datadog \
  --wait --timeout 5m

# Verify webhook is registered
kubectl get mutatingwebhookconfigurations | grep datadog
# Expected: datadog-webhook
```

### Step 3 — Build Multi-Arch Images

GKE nodes run `linux/amd64`. Mac M-series builds `arm64` by default. Use buildx to produce
fat manifests that work on both:

```bash
# One-time builder setup
docker buildx create --use --name multiarch-builder
docker buildx inspect --bootstrap

# Build and push all services (or use ./build.sh)
for svc in policy-service claims-service customer-service \
           premium-calculator-service notification-service api-gateway; do
  docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --tag wwongpai/insurance-${svc}:1.0.3 \
    --push \
    ./services/${svc}
done

docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --tag wwongpai/insurance-web-app:1.0.3 \
  --push \
  ./services/web-app

# orchestrion-demo (separate service)
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --tag wwongpai/orchestrion-demo:1.0.0 \
  --push \
  ./services/orchestrion-demo
```

### Step 4 — Deploy Infrastructure

```bash
kubectl apply -f k8s/postgres.yaml
kubectl apply -f k8s/redis.yaml
kubectl wait --for=condition=available deployment/postgres deployment/redis \
  -n insurance-app --timeout=120s
```

### Step 5 — Deploy Application Services

```bash
kubectl apply -f k8s/policy-service.yaml
kubectl apply -f k8s/claims-service.yaml
kubectl apply -f k8s/customer-service.yaml
kubectl apply -f k8s/premium-calculator-service.yaml
kubectl apply -f k8s/notification-service.yaml
kubectl apply -f k8s/api-gateway.yaml
kubectl apply -f k8s/web-app.yaml
kubectl apply -f k8s/orchestrion-demo.yaml
kubectl apply -f k8s/hpa.yaml

kubectl rollout status deployment --all -n insurance-app --timeout=5m
```

### Step 6 — Run Load Test

```bash
kubectl apply -f k8s/k6-job.yaml
kubectl wait --for=condition=complete job/k6-load-test -n insurance-app --timeout=15m
kubectl logs -n insurance-app job/k6-load-test
```

---

## 6. Critical APM Gotchas

These are real issues discovered during this project that are not obvious from documentation.

### Gotcha 1 — Agent 7.76+ Drops Raw Spans by Default

**Symptom:** Services send traces, but the Datadog Agent shows `Traces: 0 payloads, 0 traces`.
No spans appear in Datadog UI even though the agent is receiving traffic.

**Root cause:** Datadog Agent 7.76.1 introduced `compute_stats_by_span_kind=true` as a
default. When enabled, the trace-agent computes APM stats from incoming spans (for error
rates, p99 latency) and then **drops the raw span payloads** instead of forwarding them to
Datadog. The Datadog UI shows dashboards but no individual traces.

**Fix:** Add to `datadog-values.yaml` under `agents.containers.traceAgent.env`:

```yaml
- name: DD_APM_COMPUTE_STATS_BY_SPAN_KIND
  value: "false"
- name: DD_APM_PEER_TAGS_AGGREGATION
  value: "false"
```

Then apply: `helm upgrade datadog datadog/datadog -f datadog-values.yaml -n datadog`

### Gotcha 2 — Admission Controller Requires a LABEL, Not an Annotation

**Symptom:** `DD_AGENT_HOST` is not injected even though the namespace label is set.

**Root cause:** The Datadog webhook's `objectSelector` matches on **pod labels**. If you
place `admission.datadoghq.com/enabled: "true"` in the pod template's `annotations` block
instead of the `labels` block, the selector never matches and the webhook silently skips
the pod.

```yaml
# WRONG — annotation, webhook does not see this
spec:
  template:
    metadata:
      annotations:
        admission.datadoghq.com/enabled: "true"   # ← WRONG

# CORRECT — label, webhook objectSelector matches this
spec:
  template:
    metadata:
      labels:
        admission.datadoghq.com/enabled: "true"   # ← CORRECT
```

### Gotcha 3 — Orchestrion Requires Go 1.25

**Symptom:** `orchestrion pin` or `go mod tidy` fails with:

```
dd-trace-go/orchestrion/all/v2@v2.7.0 requires go >= 1.25.0
```

**Fix:** Update both `go.mod` and the `FROM` line in the Dockerfile:

```
# go.mod
go 1.25

# Dockerfile
FROM golang:1.25 AS builder
```

### Gotcha 4 — Pods Must Be Created After the Webhook is Registered

The Admission Controller only mutates pods **at creation time**. If pods were already running
before the webhook was registered, they will not have `DD_AGENT_HOST` injected.

```bash
# Force all pods to restart so they are mutated by the webhook
kubectl rollout restart deployment --all -n insurance-app
```

---

## 7. Validation

### Confirm Admission Controller Injected DD_* Vars

```bash
kubectl exec -n insurance-app deployment/policy-service -- env | sort | grep ^DD_
```

Expected output (must include these):
```
DD_AGENT_HOST=10.148.x.x
DD_ENV=demo2
DD_SERVICE=policy-service
DD_TRACE_AGENT_URL=http://10.148.x.x:8126
DD_VERSION=1.0.3
```

### Confirm Orchestrion Instrumented the Binary

```bash
kubectl logs -n insurance-app deployment/api-gateway | grep orchestrion
```

Expected: `"orchestrion":{"enabled":true,"metadata":{"version":"v1.8.0"}}`

### Confirm Agent is Forwarding Traces

```bash
kubectl logs -n datadog -l app=datadog-agent -c agent | grep "Traces:"
```

Expected: `Traces: N payloads, N traces, N bytes` (non-zero values)

### API Smoke Test

```bash
GW=$(kubectl get svc api-gateway-service -n insurance-app \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

curl -sf http://$GW/health | jq .
curl -sf -X POST http://$GW/api/v1/customers \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -d '{"firstName":"Test","lastName":"User","email":"test@example.invalid","phone":"+15551234567","dateOfBirth":"1990-01-01"}' | jq .
```

### Orchestrion Demo Smoke Test

```bash
DEMO=$(kubectl get svc orchestrion-demo-service -n insurance-app \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "use port-forward")

# If no LoadBalancer, use port-forward:
kubectl port-forward svc/orchestrion-demo-service 8090:80 -n insurance-app &
curl http://localhost:8090/hello?name=Orchestrion
curl http://localhost:8090/fibonacci/42
curl http://localhost:8090/work/200
```

---

## 8. Troubleshooting

| Problem | Check | Fix |
|---|---|---|
| `DD_AGENT_HOST` missing from pod env | `kubectl exec ... -- env \| grep DD_` | Check label vs annotation (Gotcha 2). Restart pods after fixing. |
| Agent shows `Traces: 0 payloads` | `kubectl logs -n datadog ... -c agent \| grep Traces` | Add `DD_APM_COMPUTE_STATS_BY_SPAN_KIND=false` (Gotcha 1). Helm upgrade. |
| `exec format error` on pod start | `kubectl describe pod ... \| grep Events` | Image built for wrong arch. Rebuild with `--platform linux/amd64,linux/arm64`. |
| `orchestrion pin` fails during docker build | Build logs | Update to `golang:1.25` and `go 1.25` in go.mod (Gotcha 3). |
| Traces appear but service names are wrong | DD UI APM view | Check `tags.datadoghq.com/service` label on pod template. |
| No traces after Helm upgrade | Agent logs | Restart app pods — they need to reconnect to the updated agent. |

---

## 9. Cleanup

```bash
# Remove application workloads
kubectl delete namespace insurance-app

# Remove Datadog
helm uninstall datadog -n datadog
kubectl delete namespace datadog
kubectl delete mutatingwebhookconfigurations datadog-webhook 2>/dev/null || true

# Remove local buildx builder (optional)
docker buildx rm multiarch-builder
```

---

## Repository Structure

```
.
├── README.md                        ← this file
├── DEPLOYMENT_GUIDE.md              ← detailed deployment reference
├── datadog-values.yaml              ← Datadog Helm chart values
├── build.sh                         ← multi-arch image build script
├── deploy.sh                        ← kubectl apply script
│
├── services/
│   ├── api-gateway/                 ← Gin, JWT stub, request routing
│   ├── policy-service/              ← Gin, PostgreSQL
│   ├── claims-service/              ← Gin, PostgreSQL + Redis
│   ├── customer-service/            ← Gin, PostgreSQL
│   ├── premium-calculator-service/  ← net/http stdlib, Redis
│   ├── notification-service/        ← net/http stdlib, Redis
│   ├── orchestrion-demo/            ← minimal demo: pure stdlib, no dd-trace imports
│   └── web-app/                     ← nginx static frontend
│
└── k8s/
    ├── namespace.yaml               ← insurance-app namespace (with Admission Controller label)
    ├── postgres.yaml
    ├── redis.yaml
    ├── api-gateway.yaml
    ├── policy-service.yaml
    ├── claims-service.yaml
    ├── customer-service.yaml
    ├── premium-calculator-service.yaml
    ├── notification-service.yaml
    ├── web-app.yaml
    ├── orchestrion-demo.yaml
    ├── hpa.yaml
    ├── k6-job.yaml
    └── secrets-template.txt         ← reference only — never applied automatically
```
