# Dockerfile

# Build stage
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN go build -o router ./cmd/router

# Final stage
FROM alpine:3.22.2
WORKDIR /app
COPY --from=builder /app/router .
COPY pools.yaml .
EXPOSE 9090
ENTRYPOINT ["/app/router"]