# ---- build stage ----
FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/untis-server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/untis-seed ./cmd/seed && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/untis-perm ./cmd/perm && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/untisctl ./cmd/untisctl

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
    UNTIS_VERSION=dev \
    UNTIS_POLL_INTERVAL=60s \
    UNTIS_NTFY_BASE=https://ntfy.sh \
    UNTIS_RECON_REFRESH=21

EXPOSE 8509
CMD ["sh", "-c", "untis-server -addr \"$UNTIS_ADDR\" -db \"$UNTIS_DB\" -env \"$UNTIS_ENV\" -version \"$UNTIS_VERSION\" -poll-interval \"$UNTIS_POLL_INTERVAL\" -recon-refresh \"$UNTIS_RECON_REFRESH\""]
