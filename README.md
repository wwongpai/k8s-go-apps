# k8s-go-apps

A sample Go microservices application running on a Kubernetes cluster (GKE) with **Datadog** for full-stack observability — metrics, distributed traces, and logs — using [Orchestrion](https://datadoghq.dev/orchestrion/) for **compile-time APM instrumentation**.

---

## Table of Contents

1. [Purpose](#1-purpose)
2. [Architecture](#2-architecture)
3. [What is Orchestrion?](#3-what-is-orchestrion)
4. [How Orchestrion Works — and How This Repo Uses It](#4-how-orchestrion-works--and-how-this-repo-uses-it)
5. [Manual Instrumentation (Fallback)](#5-manual-instrumentation-fallback)
6. [Troubleshooting](#6-troubleshooting)
7. [Cleanup](#7-cleanup)

---

## 1. Purpose

This repository demonstrates how to run Go microservices on Kubernetes with Datadog collecting:

- **Metrics** — infrastructure, pod, and container metrics via the Datadog DaemonSet Agent
- **Distributed Traces** — end-to-end APM traces across all services via Orchestrion compile-time instrumentation
- **Logs** — container logs collected and correlated with traces via the DaemonSet Agent

The key design goal is **zero source-code changes for APM**. No `tracer.Start()`, no middleware wrappers, no traced client wrappers. Orchestrion injects all instrumentation at compile time, keeping application code plain, idiomatic Go.

---

## 2. Architecture

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
│                 └─────────────┘───►┌──────────────────┐                  │
│                                    │ notif-service    │──► Redis         │
│                                    │ :8085  (stdlib)  │                  │
│                                    └──────────────────┘                  │
│                                                                            │
│  ┌──────────────────────────────────────────────────────────────────┐    │
│  │  Datadog DaemonSet Agent (port 8126) — one pod per node          │    │
│  │  APM traces (TCP) ◄── all services via DD_AGENT_HOST=<nodeIP>    │    │
│  │  Metrics + Logs ◄── collected from all containers on the node    │    │
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

**Datadog components:**

```
┌─────────────────────────────────── datadog namespace ───────────────────┐
│                                                                           │
│  ┌─────────────────────────────┐   ┌──────────────────────────────┐     │
│  │  Datadog Cluster Agent      │   │  Datadog DaemonSet Agent     │     │
│  │  (2 replicas for HA)        │   │  (1 pod per node)            │     │
│  │                             │   │                              │     │
│  │  - Admission Controller     │   │  - Receives APM traces :8126 │     │
│  │  - MutatingWebhook          │   │  - Collects container logs   │     │
│  │  - Cluster-level metrics    │   │  - Node-level metrics        │     │
│  └─────────────────────────────┘   └──────────────────────────────┘     │
└───────────────────────────────────────────────────────────────────────────┘
```

The **Admission Controller** (part of the Cluster Agent) is a MutatingAdmissionWebhook that intercepts pod creation and automatically injects `DD_AGENT_HOST`, `DD_SERVICE`, `DD_ENV`, and `DD_VERSION` environment variables into every app container — no hardcoded values in manifests.

---

## 3. What is Orchestrion?

[Orchestrion](https://datadoghq.dev/orchestrion/) is a **Go toolchain proxy** developed by Datadog. Instead of requiring developers to manually import tracing libraries and wrap every HTTP handler, database call, and Redis client, Orchestrion intercepts the Go compilation pipeline, parses the AST of every source file, detects calls to supported libraries, and injects tracing code before the real compiler sees the file.

The result is a fully instrumented binary with **no Datadog imports and no code changes** in your application.

### Version Compatibility

The table below is sourced from the actual `go.mod` of each Orchestrion release on GitHub:

| Orchestrion Version | Minimum Go Version | dd-trace-go Version |
|---|---|---|
| v1.6.x – v1.8.x | **Go 1.24** | v2.x |
| v1.1.x – v1.5.x | **Go 1.23** | v1.72+ / v2.x |
| v1.0.x | **Go 1.22** | v1.x |
| v0.9.x | **Go 1.22** | v1.x |

> **This repo uses Orchestrion v1.8.0 + dd-trace-go v2.6.0, with services set to Go 1.25.**
> Orchestrion v1.8.0 requires Go 1.24 at minimum. If you are on an older Go version, use the matching Orchestrion release from the table above.

Datadog's support policy: the **two latest Go releases are fully supported**; the third newest is in maintenance mode. See [compatibility requirements](https://docs.datadoghq.com/tracing/compatibility_requirements/go/) for details.

### What Gets Automatically Instrumented

| Library | What Gets Traced |
|---|---|
| `net/http` server | Every `HandleFunc` / `Handle` → span per inbound request |
| `net/http` client | Every `http.Client.Do` / `Get` / `Post` → span per outbound call |
| `github.com/gin-gonic/gin` | Every route and middleware chain → span per matched route |
| `database/sql` | `Query`, `Exec`, `Begin`, `Commit`, `Rollback` → span with sanitized SQL |
| `github.com/redis/go-redis/v9` | Every Redis command → span per command |
| `github.com/go-redis/redis/v8` | Same as above for v8 API |
| `google.golang.org/grpc` | Server and client interceptors → span per RPC |
| Go standard library | DNS, TLS, file I/O, and more |

---

## 4. How Orchestrion Works — and How This Repo Uses It

### How It Works

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
  Final Binary — APM built in
```

### Official Setup Steps (from Datadog docs)

**Step 1 — Install the Orchestrion CLI:**

```sh
go install github.com/DataDog/orchestrion@latest
```

**Step 2 — Register Orchestrion as a tool dependency in your project:**

```sh
orchestrion pin
```

This creates `orchestrion.tool.go` and updates `go.mod` / `go.sum`.

**Step 3 — Commit the generated files:**

```sh
git add go.mod go.sum orchestrion.tool.go
git commit -m "chore: enable orchestrion"
```

**Step 4 — Build with Orchestrion instead of `go build`:**

```sh
# Option A — orchestrion as prefix (used in this repo's Dockerfiles)
orchestrion go build .

# Option B — via -toolexec flag
go build -toolexec="orchestrion toolexec" .

# Option C — via environment variable (CI/CD pipelines)
export GOFLAGS="${GOFLAGS} '-toolexec=orchestrion toolexec'"
go build .
```

**Step 5 — Configure Unified Service Tagging via environment variables:**

| Tag | Environment Variable |
|---|---|
| Service name | `DD_SERVICE` |
| Environment | `DD_ENV` |
| Version | `DD_VERSION` |

In Kubernetes, these are injected automatically by the Datadog Admission Controller (see below).

### How This Repo Implements It

#### Dockerfile pattern (all 6 microservices):

```dockerfile
FROM golang:1.25 AS builder
WORKDIR /app

# Install Orchestrion CLI
RUN go install github.com/DataDog/orchestrion@latest

COPY go.mod ./
COPY *.go ./

# Pin Orchestrion as a tool dependency
RUN orchestrion pin && go mod tidy

# Build with Orchestrion — injects tracing at compile time
RUN orchestrion go build -o /app/service .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/service /service
EXPOSE 8080
ENTRYPOINT ["/service"]
```

The only differences from a standard Dockerfile are:
1. `go install github.com/DataDog/orchestrion@latest`
2. `orchestrion pin && go mod tidy`
3. `orchestrion go build` instead of `go build`

#### Kubernetes manifest pattern (all deployments):

```yaml
spec:
  template:
    metadata:
      labels:
        app: policy-service
        # Unified Service Tagging — Admission Controller reads these and injects DD_* env vars
        tags.datadoghq.com/env: "demo2"
        tags.datadoghq.com/service: "policy-service"
        tags.datadoghq.com/version: "1.0.3"
        # IMPORTANT: Must be a LABEL (not an annotation) for the webhook to trigger
        admission.datadoghq.com/enabled: "true"
```

The Admission Controller injects into every container at pod creation time:

```
DD_AGENT_HOST       = <node-ip>
DD_TRACE_AGENT_URL  = http://<node-ip>:8126
DD_ENV              = demo2
DD_SERVICE          = policy-service
DD_VERSION          = 1.0.3
```

The Orchestrion-compiled binary reads these at startup and initializes the Datadog tracer automatically — no `tracer.Start()` call needed in code.

#### Adding custom spans for business logic:

For your own functions, add a `//dd:span` comment directly before the function. The function's first parameter must be `context.Context`:

```go
//dd:span policy.type:auto component:risk-engine
func calculateAutoRisk(ctx context.Context, req AutoRiskRequest) (RiskScore, error) {
    // Orchestrion wraps this entire function in a span named "calculateAutoRisk"
    // with tags: policy.type=auto, component=risk-engine
    return computeScore(req), nil
}
```

Errors returned by annotated functions are automatically attached to the span:

```go
//dd:span
func failableFunction() (any, error) {
    return nil, errors.ErrUnsupported  // error auto-attached to span
}
```

#### What NOT to do when using Orchestrion:

| Anti-pattern | Why it breaks things |
|---|---|
| `tracer.Start(tracer.WithService(...))` | Orchestrion injects `tracer.Start()` automatically — calling it again creates a conflicting second tracer |
| `gintrace.New()` middleware | Orchestrion already instruments Gin — this creates duplicate spans |
| `sqltrace.Register("postgres", ...)` | Orchestrion already instruments `database/sql` — duplicate spans |
| `redistrace.NewClient(...)` | Orchestrion already instruments go-redis — duplicate spans |

Write plain, idiomatic Go. Use `//dd:span` only for your own business logic functions.

---

## 5. Manual Instrumentation (Fallback)

If Orchestrion is not suitable for your project (unsupported Go version, vendoring constraints, etc.), use the [Datadog Go tracing library](https://docs.datadoghq.com/tracing/trace_collection/dd_libraries/go/?tab=manualinstrumentation) directly.

### Step 1 — Add the tracer library

```sh
go get github.com/DataDog/dd-trace-go/v2/ddtrace/tracer
```

### Step 2 — Start and stop the tracer in `main()`

```go
import "github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"

func main() {
    tracer.Start(
        tracer.WithService("policy-service"),
        tracer.WithEnv("production"),
        tracer.WithServiceVersion("1.0.0"),
    )
    defer tracer.Stop()

    // ... rest of main
}
```

### Step 3 — Instrument HTTP handlers (Gin)

```go
import gintrace "github.com/DataDog/dd-trace-go/v2/contrib/gin-gonic/gin"

r := gin.New()
r.Use(gintrace.Middleware("policy-service"))
```

### Step 4 — Instrument `database/sql`

```go
import (
    sqltrace "github.com/DataDog/dd-trace-go/v2/contrib/database/sql"
    _ "github.com/lib/pq"
)

sqltrace.Register("postgres", &pq.Driver{}, sqltrace.WithServiceName("postgres"))
db, err := sqltrace.Open("postgres", dsn)
```

### Step 5 — Instrument Redis

```go
import redistrace "github.com/DataDog/dd-trace-go/v2/contrib/redis/go-redis.v9"

rdb := redistrace.NewClient(&redis.Options{Addr: "localhost:6379"})
```

### Step 6 — Instrument outbound HTTP calls

```go
import httptrace "github.com/DataDog/dd-trace-go/v2/contrib/net/http"

client := httptrace.WrapClient(http.DefaultClient)
resp, err := client.Get("http://downstream-service/api")
```

### Step 7 — Create custom spans

```go
import "github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"

func processPayment(ctx context.Context, amount float64) error {
    span, ctx := tracer.StartSpanFromContext(ctx, "payment.process")
    defer span.Finish()

    span.SetTag("payment.amount", amount)

    if err := chargeCard(ctx, amount); err != nil {
        span.Finish(tracer.WithError(err))
        return err
    }
    return nil
}
```

### Comparison: Orchestrion vs Manual

| | Orchestrion | Manual |
|---|---|---|
| Source code changes | None | Import + wrap every library |
| HTTP server tracing | Automatic | Add middleware per router |
| Database tracing | Automatic | Replace `sql.Open` with traced version |
| Redis tracing | Automatic | Replace `redis.NewClient` with traced version |
| Custom business spans | `//dd:span` comment | `tracer.StartSpanFromContext()` |
| Go version requirement | Two latest releases | Go 1.18+ |
| Maintenance overhead | Low (Orchestrion updates) | Higher (manual updates per integration) |

---

## 6. Troubleshooting

### Quick diagnostic commands

```bash
# 1. Confirm Admission Controller injected DD_* vars into a pod
kubectl exec -n insurance-app deployment/policy-service -- env | sort | grep ^DD_
# Must include: DD_AGENT_HOST, DD_ENV, DD_SERVICE, DD_TRACE_AGENT_URL, DD_VERSION

# 2. Confirm Orchestrion is active in the binary
kubectl logs -n insurance-app deployment/api-gateway | grep orchestrion
# Expected: "orchestrion":{"enabled":true,"metadata":{"version":"v1.8.0"}}

# 3. Confirm the Datadog Agent is receiving and forwarding traces
kubectl logs -n datadog -l app=datadog-agent -c agent | grep "Traces:"
# Expected: Traces: N payloads, N traces, N bytes  (non-zero)

# 4. Check the MutatingWebhook is registered
kubectl get mutatingwebhookconfigurations | grep datadog
```

### Common problems

| Problem | Symptom | Fix |
|---|---|---|
| `DD_AGENT_HOST` missing from pod | `env \| grep DD_` returns nothing | Check that `admission.datadoghq.com/enabled: "true"` is a **label** (not annotation) on the pod template. Restart pods after fixing. |
| Agent shows `Traces: 0 payloads` | No traces appear in Datadog UI | Add `DD_APM_COMPUTE_STATS_BY_SPAN_KIND=false` and `DD_APM_PEER_TAGS_AGGREGATION=false` to the trace-agent env in `datadog-values.yaml`, then run `helm upgrade`. |
| `orchestrion pin` fails during Docker build | `dd-trace-go/orchestrion/all/v2@v2.7.0 requires go >= 1.25.0` | Update `go.mod` to `go 1.25` and the Dockerfile `FROM` line to `golang:1.25`. |
| `exec format error` on pod start | Pod crashes immediately | Image built for wrong architecture. Rebuild with `--platform linux/amd64,linux/arm64` using `docker buildx`. |
| Traces appear but service names are wrong | Wrong service name in Datadog APM | Check `tags.datadoghq.com/service` label on the pod template spec. |
| No traces after Helm upgrade | Traces stop appearing | Restart app pods — they need to reconnect to the updated agent: `kubectl rollout restart deployment --all -n insurance-app`. |
| Pods existed before Datadog was installed | `DD_AGENT_HOST` not injected | The Admission Controller only mutates pods at creation time. Delete and recreate pods after the webhook is registered. |

### Agent 7.76+ drops raw spans — the most common issue

Datadog Agent 7.76+ introduced `compute_stats_by_span_kind=true` as a default. When enabled, the agent computes APM statistics from incoming spans and **drops the raw span payloads**, so the Datadog UI shows no individual traces.

Add this to `datadog-values.yaml` and re-apply:

```yaml
agents:
  containers:
    traceAgent:
      env:
        - name: DD_APM_COMPUTE_STATS_BY_SPAN_KIND
          value: "false"
        - name: DD_APM_PEER_TAGS_AGGREGATION
          value: "false"
```

```bash
helm upgrade datadog datadog/datadog -f datadog-values.yaml -n datadog
kubectl rollout restart deployment --all -n insurance-app
```

---

## 7. Cleanup

```bash
# Remove all application workloads and the namespace
kubectl delete namespace insurance-app

# Remove Datadog
helm uninstall datadog -n datadog
kubectl delete namespace datadog
kubectl delete mutatingwebhookconfigurations datadog-webhook 2>/dev/null || true

# Remove local buildx builder (optional)
docker buildx rm multiarch-builder
```
