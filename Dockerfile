# syntax=docker/dockerfile:1.29@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183

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

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
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
