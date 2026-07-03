# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT

###############
# Build stage #
###############
# The builder runs natively on the BUILD platform (the amd64 GitHub runner) and
# cross-compiles the Go binary for the TARGET arch. CGO is disabled, so this is a
# fast pure-Go cross-compile with no QEMU emulation. Buildx injects the
# BUILDPLATFORM / TARGETOS / TARGETARCH args automatically.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /go/src/app

# Cache dependency downloads (arch-independent)
COPY go.mod go.sum ./
RUN go mod download

# Cross-compile for the target arch
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /go/bin/argobot .

#################
# Runtime stage #
#################
# Target-arch image. It contains NO RUN steps, so QEMU is only ever used to
# assemble/pull layers — it never executes an emulated binary. CA certs are
# copied from the builder (a PEM bundle is arch-independent); the numeric USER
# needs no /etc/passwd entry.
FROM alpine:latest
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 1000:3000
WORKDIR /app
COPY --from=builder /go/bin/argobot .
ENTRYPOINT ["/app/argobot"]
