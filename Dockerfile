# Build context is the repo root. (It was operator/ while this lived inside the
# K8Boss monorepo; the operator is its own Go module, which is what let it be
# split out — ADR-0003 §1.)
# Must satisfy the `go` directive in go.mod. The official golang images set
# GOTOOLCHAIN=local, so a builder older than that directive does NOT silently
# download a newer toolchain — it fails `go mod download` outright. Dependabot
# bumped go.mod 1.23 -> 1.25.0 without touching this line and the image stopped
# building, while a non-fail-fast build matrix let sibling images keep
# publishing, so nothing announced it. Bump both together.
# --platform=$BUILDPLATFORM pins the builder stage to the NATIVE architecture of
# the machine running the build, and it is load-bearing, not decoration.
#
# Without it, a `--platform linux/amd64,linux/arm64` build runs this entire
# stage under QEMU emulation for the non-native target — so `go build` would
# cross-compile to arm64 from an *emulated arm64* builder, which is both
# pointless and pathologically slow: the first publish run sat on this step for
# well over 13 minutes compiling the standard library under emulation.
#
# Pinning the builder native and letting GOARCH below do the cross-compile is
# the whole reason TARGETARCH exists, and is the upstream kubebuilder pattern.
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
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
#
# No `-a`. It forces a rebuild of every package including the standard library,
# which in a fresh container buys nothing — the build cache starts empty anyway —
# while doubling the work on a two-platform build.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -o manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
