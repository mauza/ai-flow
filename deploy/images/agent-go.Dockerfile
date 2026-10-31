# agent-base plus the Go toolchain and a C compiler (cgo, so `go test -race`
# works: a web-games check asked for it and failed with "-race requires cgo").
# Caches live under /work (writable).
ARG BASE=ai-flow-agent:dev
FROM golang:1.26-bookworm AS go
FROM ${BASE}
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*
USER 1000:1000
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/work/.go/bin:$PATH \
    GOPATH=/work/.go \
    GOCACHE=/work/.cache/go-build \
    GOTOOLCHAIN=local \
    GOFLAGS=-buildvcs=false
