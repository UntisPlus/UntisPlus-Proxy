# ---- build stage ----
# Run the compiler on the native builder even when the target is arm64. CGO is
# disabled, so Go can cross-compile; this avoids emulating the whole sqlite
# dependency tree under QEMU and is the difference between a ~6 minute and a
# ~1 minute arm64 image build.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src

ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# The version is compiled in, so the image knows what it is without a runtime
# flag or environment variable that could disagree with the tag it is published
# under. Plain `docker build .` leaves it "dev".
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /out/untis-server ./cmd/server && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/untis-seed ./cmd/seed && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/untis-perm ./cmd/perm && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/untisctl ./cmd/untisctl

# ---- runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/

VOLUME /data
# Every one of these is also honoured as a plain env var by the binary itself
# (UNTIS_NTFY_BASE / UNTIS_PUBLIC_BASE included), so the CMD below only has to
# carry the flags it wants to be explicit about.
ENV UNTIS_ADDR=:8509 \
    UNTIS_SERVER="" \
    UNTIS_SCHOOL="" \
    UNTIS_DB=/data/untis.db \
    UNTIS_ENV=prod \
    UNTIS_POLL_INTERVAL=60s \
    UNTIS_NTFY_BASE=https://ntfy.sh \
    UNTIS_RECON_REFRESH=21

EXPOSE 8509
CMD ["sh", "-c", "untis-server -addr \"$UNTIS_ADDR\" -db \"$UNTIS_DB\" -env \"$UNTIS_ENV\" -poll-interval \"$UNTIS_POLL_INTERVAL\" -recon-refresh \"$UNTIS_RECON_REFRESH\""]
