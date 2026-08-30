# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.0-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd/manager
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ptah-runner ./cmd/ptah-runner

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /out/manager /manager
COPY --from=builder /out/ptah-runner /ptah-runner
USER 65532:65532
ENTRYPOINT ["/manager"]
