# syntax=docker/dockerfile:1.29@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183

# The capacity driver's toolbox: the capacity tool built from the committed
# tree it is given as context, Python for the probes beside it, the kubectl
# shipped in the lab's own kind node image, and the pinned Ptah the database
# checks call. It runs inside the control-plane node's network namespace, so a
# multi-hour measurement does not depend on the machine that started it.
ARG NODE_IMAGE
FROM ${NODE_IMAGE} AS node

FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder
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

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache python3
COPY --from=builder /out/capacity /out/ptah /usr/local/bin/
COPY --from=node /usr/bin/kubectl /usr/local/bin/kubectl
COPY . /src
WORKDIR /work
CMD ["sleep", "infinity"]
