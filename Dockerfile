FROM golang:1.22 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace

# Download modules first so source changes do not invalidate this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd

# Distroless static: no shell, no package manager, runs as non-root.
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
