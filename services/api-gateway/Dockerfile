FROM golang:1.25 AS builder
WORKDIR /app
RUN go install github.com/DataDog/orchestrion@latest
COPY go.mod ./
COPY *.go ./
RUN orchestrion pin && go mod tidy
RUN orchestrion go build -o /app/service .
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/service /service
EXPOSE 8080
ENTRYPOINT ["/service"]
