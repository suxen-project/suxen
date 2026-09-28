# syntax=docker/dockerfile:1
# Build the pinned upstream source releases. Their Go modules are verified by
# the public Go checksum database; no MinIO container registry is required.
FROM golang:1.26-alpine@sha256:51a7c389a5ddaf82f527191a1e9bff9928655130a44e4975dd1d7e0acf59f1ae AS build-minio
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
# Official RELEASE.2025-09-07T16-13-09Z, commit 07c3a429bfed433e49018cb0f78a52145d4bedeb.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install -trimpath \
    -ldflags="-s -w \
      -X github.com/minio/minio/cmd.Version=2025-09-07T16:13:09Z \
      -X github.com/minio/minio/cmd.CopyrightYear=2025 \
      -X github.com/minio/minio/cmd.ReleaseTag=RELEASE.2025-09-07T16-13-09Z \
      -X github.com/minio/minio/cmd.CommitID=07c3a429bfed433e49018cb0f78a52145d4bedeb \
      -X github.com/minio/minio/cmd.ShortCommitID=07c3a429bfed" \
    github.com/minio/minio@v0.0.0-20250907161309-07c3a429bfed

FROM golang:1.26-alpine@sha256:51a7c389a5ddaf82f527191a1e9bff9928655130a44e4975dd1d7e0acf59f1ae AS build-mc
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
# Official RELEASE.2025-08-13T08-35-41Z, commit 7394ce0dd2a80935aded936b09fa12cbb3cb8096.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install -trimpath \
    -ldflags="-s -w \
      -X github.com/minio/mc/cmd.Version=2025-08-13T08:35:41Z \
      -X github.com/minio/mc/cmd.CopyrightYear=2025 \
      -X github.com/minio/mc/cmd.ReleaseTag=RELEASE.2025-08-13T08-35-41Z \
      -X github.com/minio/mc/cmd.CommitID=7394ce0dd2a80935aded936b09fa12cbb3cb8096 \
      -X github.com/minio/mc/cmd.ShortCommitID=7394ce0dd2a8" \
    github.com/minio/mc@v0.0.0-20250813083541-7394ce0dd2a8

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS minio
RUN apk add --no-cache ca-certificates curl
COPY --from=build-minio /go/bin/minio /usr/local/bin/minio
EXPOSE 9000
VOLUME ["/data"]
ENTRYPOINT ["minio"]
CMD ["server", "/data"]

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS mc
RUN apk add --no-cache ca-certificates
COPY --from=build-mc /go/bin/mc /usr/local/bin/mc
ENTRYPOINT ["mc"]
