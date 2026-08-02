# Stage 1: Build binary
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy dependency manifests
COPY go.mod go.sum ./

# Copy source code
COPY . .

# Set GOTOOLCHAIN=auto to allow automatic toolchain management if needed
ENV GOTOOLCHAIN=auto

# Build statically linked binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /bot cmd/bot/main.go

# Stage 2: Lightweight final image
FROM alpine:3.20

WORKDIR /app

# Install CA certificates for HTTPS requests to api.telegram.org
RUN apk --no-cache add ca-certificates

# Copy binary from builder
COPY --from=builder /bot /app/bot

# Copy locale files
COPY --from=builder /app/locales /app/locales

EXPOSE 8080

ENTRYPOINT ["/app/bot"]
