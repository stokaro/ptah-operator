# syntax=docker/dockerfile:1.28@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b

# The capacity driver's toolbox: the capacity tool built from the committed
# tree it is given as context, Python for the probes beside it, the kubectl
# shipped in the lab's own kind node image, and the pinned Ptah the database
# checks call. It runs inside the control-plane node's network namespace, so a
# multi-hour measurement does not depend on the machine that started it.
ARG NODE_IMAGE
FROM ${NODE_IMAGE} AS node

FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder
ARG PTAH_COMMIT
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/capacity ./hack/capacity
RUN git clone --filter=blob:none --no-checkout https://github.com/stokaro/ptah /ptah && \
    git -C /ptah checkout --detach "$PTAH_COMMIT" && \
    test "$(git -C /ptah rev-parse HEAD)" = "$PTAH_COMMIT" && \
    cd /ptah && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ptah ./cmd/ptah

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache python3
COPY --from=builder /out/capacity /out/ptah /usr/local/bin/
COPY --from=node /usr/bin/kubectl /usr/local/bin/kubectl
COPY . /src
WORKDIR /work
CMD ["sleep", "infinity"]
