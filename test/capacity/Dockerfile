# syntax=docker/dockerfile:1
FROM golang:1.26-alpine@sha256:51a7c389a5ddaf82f527191a1e9bff9928655130a44e4975dd1d7e0acf59f1ae AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY test/capacity ./test/capacity
RUN CGO_ENABLED=0 go build -trimpath -o /out/capacity ./test/capacity

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
COPY --from=build /out/capacity /usr/local/bin/capacity
ENTRYPOINT ["/usr/local/bin/capacity"]
