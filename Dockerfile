# Build context is the repo root. (It was operator/ while this lived inside the
# K8Boss monorepo; the operator is its own Go module, which is what let it be
# split out — ADR-0003 §1.)
# Must satisfy the `go` directive in go.mod. The official golang images set
# GOTOOLCHAIN=local, so a builder older than that directive does NOT silently
# download a newer toolchain — it fails `go mod download` outright. Dependabot
# bumped go.mod 1.23 -> 1.25.0 without touching this line and the image stopped
# building, while a non-fail-fast build matrix let sibling images keep
# publishing, so nothing announced it. Bump both together.
FROM golang:1.25 AS builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# TARGETOS/TARGETARCH are supplied by BuildKit per target platform. They are
# load-bearing for the multi-arch build in .github/workflows/ci.yml: without
# them `go build` targets the BUILDER's architecture, so the arm64 image would
# contain an amd64 binary and fail at exec with a format error that reads like a
# corrupt build rather than a wrong platform.
ARG TARGETOS
ARG TARGETARCH

# CGO off + static: the runtime image has no libc.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
