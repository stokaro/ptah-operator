# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG REVISION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.controllerRevision=${REVISION}" -o /out/manager ./cmd/manager
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ptah-runner ./cmd/ptah-runner
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ptah-cert-rotator ./cmd/ptah-cert-rotator
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ptah-crd-manager ./cmd/ptah-crd-manager

FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
ARG VERSION=dev
ARG REVISION=unknown
ARG SOURCE=https://github.com/stokaro/ptah-operator
LABEL org.opencontainers.image.title="Ptah Operator" \
      org.opencontainers.image.description="Credential-isolated Kubernetes operator for Ptah schema convergence" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION" \
      org.opencontainers.image.source="$SOURCE"
COPY --from=builder /out/manager /manager
COPY --from=builder /out/ptah-runner /ptah-runner
COPY --from=builder /out/ptah-cert-rotator /ptah-cert-rotator
COPY --from=builder /out/ptah-crd-manager /ptah-crd-manager
USER 65532:65532
ENTRYPOINT ["/manager"]
