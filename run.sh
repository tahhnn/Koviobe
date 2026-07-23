#!/bin/bash
if command -v go >/dev/null 2>&1; then
    go run cmd/api-gateway/main.go
else
    ./api-gateway
fi