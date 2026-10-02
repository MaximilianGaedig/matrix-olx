FROM golang:1-alpine3.24 AS builder

RUN apk add --no-cache git ca-certificates build-base su-exec olm-dev

COPY . /build
WORKDIR /build
RUN ./build.sh

FROM alpine:3.24

ENV UID=1337 \
    GID=1337

RUN apk add --no-cache su-exec ca-certificates olm bash jq curl yq-go

COPY --from=builder /build/matrix-olx /usr/bin/matrix-olx
COPY --from=builder /build/docker-run.sh /docker-run.sh
VOLUME /data

CMD ["/docker-run.sh"]
