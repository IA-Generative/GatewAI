# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.27-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath \
    -o /app/gateway ./cmd/gateway

# ── Runtime stage ─────────────────────────────────────────────────────────────
# distroless/static includes CA certificates (needed for HTTPS MinIO/S3 + webhooks)
FROM gcr.io/distroless/static-debian13:nonroot

WORKDIR /app

COPY --from=builder /app/gateway .
COPY config.yaml .

EXPOSE 8080

ENTRYPOINT ["/app/gateway"]
