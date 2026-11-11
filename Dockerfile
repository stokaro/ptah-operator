# syntax=docker/dockerfile:1.28@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b

FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS builder
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

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
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
