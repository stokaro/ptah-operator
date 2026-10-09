# syntax=docker/dockerfile:1.27@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e

# The capacity driver's toolbox: the capacity tool built from the committed
# tree it is given as context, Python for the probes beside it, the kubectl
# shipped in the lab's own kind node image, and the pinned Ptah the database
# checks call. It runs inside the control-plane node's network namespace, so a
# multi-hour measurement does not depend on the machine that started it.
ARG NODE_IMAGE
FROM ${NODE_IMAGE} AS node

FROM golang:1.27.2-alpine@sha256:85dc1069ac644ea3c527b177303a406eb3358192816cd7f9e5848eb658851673 AS builder
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

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN apk add --no-cache python3
COPY --from=builder /out/capacity /out/ptah /usr/local/bin/
COPY --from=node /usr/bin/kubectl /usr/local/bin/kubectl
COPY . /src
WORKDIR /work
CMD ["sleep", "infinity"]
