# syntax=docker/dockerfile:1.27@sha256:bde3983e9c939224420ddaf6b784cc30e09b035a4dea01f581230c50809f372e

FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder
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

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
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
