# Build context is operator/ — the operator is its own Go module (ADR-0003 §1),
# so nothing from the repo root is needed here.
FROM golang:1.23 AS builder
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
