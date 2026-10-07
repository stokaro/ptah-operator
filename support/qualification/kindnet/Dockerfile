# Qualification-only correction to kind v0.33.0's network-policy Pod lookup.
# Keep the shipped runtime and dependency versions; change only the resolver.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ARG TARGETOS
ARG TARGETARCH
ENV GOMAXPROCS=4
RUN apk add --no-cache patch
WORKDIR /src
ADD --checksum=sha256:66ce77f5a9b2cd22d67c37a89dabb5c85b76ed85f9f2b915caeeb92c39e3e533 https://api.github.com/repos/kubernetes-sigs/kind/tarball/69b56db75a6e82dece4cdc980655d79b2b73eb77 /tmp/kind.tar.gz
ADD --checksum=sha256:e87def88a61a8987cd2eb19382a87900495402b3a766f3480588261c092ba3b5 https://api.github.com/repos/kubernetes-sigs/kube-network-policies/tarball/f67f0fb35e2b8288782bc1f22988648580dda7f8 /tmp/policies.tar.gz
RUN mkdir kind policies \
    && tar -xzf /tmp/kind.tar.gz -C kind --strip-components=1 \
    && tar -xzf /tmp/policies.tar.gz -C policies --strip-components=1
COPY pending-pod.patch /tmp/pending-pod.patch
COPY pending_pod_test.go.txt /src/policies/pkg/podinfo/pending_pod_test.go
WORKDIR /src/policies
RUN patch -p1 < /tmp/pending-pod.patch \
    && printf '%s  %s\n' 053f59b2471cb93d888a35fe348fa0ec14d009ab570f194ee421e2ec573b0f3a pkg/podinfo/ipresolver.go | sha256sum -c - \
    && go test ./pkg/podinfo
WORKDIR /src/kind/images/kindnetd
RUN go mod edit -replace=sigs.k8s.io/kube-network-policies=/src/policies \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/kindnetd ./cmd/kindnetd

FROM docker.io/kindest/kindnetd@sha256:526969f0a1a7a788a7a96f53a4ad3b51607bde7bb2841edf52a02916e8d52650
COPY --from=build /out/kindnetd /bin/kindnetd
