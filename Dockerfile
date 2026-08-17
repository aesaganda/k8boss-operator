# Build context is operator/ — the operator is its own Go module (ADR-0003 §1),
# so nothing from the repo root is needed here.
# Must satisfy the `go` directive in go.mod. The official golang images set
# GOTOOLCHAIN=local, so a builder older than that directive does NOT silently
# download a newer toolchain — it fails `go mod download` outright. Dependabot
# bumped go.mod 1.23 -> 1.25.0 in 4de2642 without touching this line, and the
# operator image stopped building; `fail-fast: false` in build-images.yml let
# the other three images keep publishing, so nothing announced it. Bump both
# together.
FROM golang:1.25 AS builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CGO off + static: the runtime image has no libc.
RUN CGO_ENABLED=0 go build -a -o manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
