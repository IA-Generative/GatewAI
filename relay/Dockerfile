FROM golang:1.27-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /relay /relay
COPY config.yaml /config.yaml

ENTRYPOINT ["/relay"]
