# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT

###############
# Build stage #
###############
FROM golang:1.25-alpine AS builder
WORKDIR /go/src/app

# Cache dependency downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a static binary
COPY . .
RUN CGO_ENABLED=0 go build -o /go/bin/argobot .

#################
# Runtime stage #
#################
FROM alpine:latest

# TLS roots for talking to ConfigHub and Argo CD over HTTPS
RUN apk add --no-cache ca-certificates

# Run as non-root
RUN addgroup -g 3000 appgroup && adduser -u 1000 -g appgroup --disabled-password --no-create-home appuser
USER 1000:3000

WORKDIR /app
COPY --from=builder /go/bin/argobot .

ENTRYPOINT ["/app/argobot"]
