# syntax=docker/dockerfile:1
# suxen-sdk: builder image for custom suxen distributions.
#
# The image carries the suxen sources and a suxen-build helper that compiles
# a suxen binary with additional compile-time plugins:
#
#   docker build -t suxen-sdk -f sdk/Dockerfile .
#   docker run --rm \
#       -v "$PWD/my-plugin:/work" -v "$PWD/dist:/out" \
#       suxen-sdk --plugin example.com/my/plugin=/work
#
# Custom builds are not an offline contract. Even a local plugin that imports
# only Suxen packages may make the Go command consult its configured module
# proxy or checksum service; third-party plugin dependencies require the same
# access unless the operator provides a complete private module/cache setup.
FROM golang:1.26-alpine@sha256:51a7c389a5ddaf82f527191a1e9bff9928655130a44e4975dd1d7e0acf59f1ae
ARG VERSION=1.0.0-rc.1
LABEL org.opencontainers.image.title="suxen-sdk" \
      org.opencontainers.image.description="Version-matched builder for custom Suxen distributions" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/suxen-project/suxen"
RUN apk add --no-cache git
WORKDIR /opt/suxen
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN install -m 0755 sdk/suxen-build /usr/local/bin/suxen-build
WORKDIR /work
ENTRYPOINT ["suxen-build"]
