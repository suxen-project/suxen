# syntax=docker/dockerfile:1
FROM golang:1.26-alpine@sha256:51a7c389a5ddaf82f527191a1e9bff9928655130a44e4975dd1d7e0acf59f1ae AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=1.0.0-rc.2
# The default image compiles every plugin in. Exclude plugins per build with
# e.g. --build-arg SUXEN_BUILD_TAGS=suxen_no_gcs,suxen_no_maven,suxen_no_git,suxen_no_go,suxen_no_cargo,suxen_no_npm,suxen_no_pypi (add noui to
# also drop the embedded UI).
ARG SUXEN_BUILD_TAGS
RUN CGO_ENABLED=0 go build \
    -tags="${SUXEN_BUILD_TAGS}" \
    -trimpath \
    -ldflags="-s -w -X github.com/suxen-project/suxen/internal/server.Version=${VERSION}" \
    -o /out/suxen \
    ./cmd/suxen

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG VERSION=1.0.0-rc.2
LABEL org.opencontainers.image.title="suxen" \
      org.opencontainers.image.description="Self-hosted artifact repository" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/suxen-project/suxen"
RUN addgroup -S -g 10001 suxen && \
    adduser -S -D -H -u 10001 -G suxen suxen && \
    mkdir -p /var/lib/suxen && \
    chown -R suxen:suxen /var/lib/suxen
COPY --from=build /out/suxen /usr/local/bin/suxen
USER 10001:10001
EXPOSE 8080
VOLUME ["/var/lib/suxen"]
ENV SUXEN_DATA=/var/lib/suxen
ENV SUXEN_LISTEN=:8080
ENTRYPOINT ["/usr/local/bin/suxen"]
CMD ["serve"]
