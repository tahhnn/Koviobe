# Stage 1: Build the Go binary
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Install git (needed for some go modules) and ca-certificates
RUN apk add --no-cache git ca-certificates

# Copy dependency files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o api-gateway ./cmd/api-gateway

# Stage 2: Run the binary in a minimal image
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app

COPY --from=builder /src/api-gateway .

EXPOSE 8082

CMD ["./api-gateway"]
