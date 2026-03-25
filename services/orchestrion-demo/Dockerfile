FROM golang:1.25 AS builder
WORKDIR /app

# Install orchestrion CLI
RUN go install github.com/DataDog/orchestrion@latest

# Copy go.mod and source
COPY go.mod ./
COPY main.go ./

# Pin orchestrion into go.mod (adds orchestrion as a go:generate tool dependency)
RUN orchestrion pin

# Download all dependencies (including dd-trace-go injected by orchestrion)
RUN go mod tidy

# Build with Orchestrion compile-time instrumentation
RUN orchestrion go build -o /app/orchestrion-demo .

# --- final image ---
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/orchestrion-demo /orchestrion-demo
EXPOSE 8090
ENTRYPOINT ["/orchestrion-demo"]
