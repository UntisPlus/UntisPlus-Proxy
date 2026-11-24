# Compile both targets with the native Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
# Ordinary layers also survive on fresh CI builders with an external layer cache.
# Keep the expensive SQLite compiler outputs independent of application changes.
RUN go mod download
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
RUN go build -trimpath modernc.org/sqlite

COPY internal/ ./internal/
COPY cmd/untisctl/ ./cmd/untisctl/
# One executable shares Go, SQLite and proxy code across all four commands.
ARG VERSION=dev
RUN go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /out/untisctl ./cmd/untisctl && \
    ln -s untisctl /out/untis-server && ln -s untisctl /out/untis-seed && ln -s untisctl /out/untis-perm

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 10001 untis && adduser -S -D -H -u 10001 -G untis untis && \
    mkdir -p /data && chown 10001:10001 /data
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/

VOLUME /data
ENV UNTIS_ADDR=:8509 \
    UNTIS_SERVER="" \
    UNTIS_SCHOOL="" \
    UNTIS_DB=/data/untis.db \
    UNTIS_ENV=prod \
    UNTIS_POLL_INTERVAL=60s \
    UNTIS_NTFY_BASE=https://ntfy.sh \
    UNTIS_RECON_REFRESH=21

USER 10001:10001
EXPOSE 8509
CMD ["untis-server"]
