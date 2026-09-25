# agent-base plus the Go toolchain. Caches live under /work (writable).
ARG BASE=ai-flow-agent:dev
FROM golang:1.25-bookworm AS go
FROM ${BASE}
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/work/.go/bin:$PATH \
    GOPATH=/work/.go \
    GOCACHE=/work/.cache/go-build \
    GOTOOLCHAIN=local \
    GOFLAGS=-buildvcs=false
