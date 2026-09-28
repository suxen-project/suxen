FROM golang:1.26-alpine@sha256:51a7c389a5ddaf82f527191a1e9bff9928655130a44e4975dd1d7e0acf59f1ae AS build
WORKDIR /src
COPY test/e2e/support/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/e2e-support $(ls *.go | grep -v '_test\.go$')

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN addgroup -S -g 10002 e2e && \
    adduser -S -D -H -u 10002 -G e2e e2e && \
    mkdir -p /state && \
    chown e2e:e2e /state
COPY --from=build /out/e2e-support /usr/local/bin/e2e-support
USER 10002:10002
ENTRYPOINT ["/usr/local/bin/e2e-support"]
