# =============================================================================
# WhatsApp Gateway — Multi-stage Dockerfile (monorepo: backend + frontend)
#
# Build a partir da raiz do monorepo:
#   docker build -f backend/Dockerfile .
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: Build
# -----------------------------------------------------------------------------
FROM golang:1.22-alpine AS builder

ENV GOTOOLCHAIN=auto \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ ./

RUN go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/gateway \
    ./cmd/api

# -----------------------------------------------------------------------------
# Stage 2: Runtime
# -----------------------------------------------------------------------------
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 appuser \
    && rm -rf /var/cache/apk/*

WORKDIR /app

COPY --from=builder /out/gateway ./gateway
COPY frontend/templates ./templates

USER appuser

EXPOSE 8080

ENV PORT=8080
ENV TEMPLATES_DIR=/app/templates

ENTRYPOINT ["./gateway"]
