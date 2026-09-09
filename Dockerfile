FROM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -p 1 \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/telegram-gateway .

FROM builder AS smoke-tests

FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS runtime

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="Telegram Gateway" \
      org.opencontainers.image.source="https://github.com/shuoqiudi/it_telegram" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates su-exec tzdata \
    && addgroup -S telegram-gateway \
    && adduser -S -D -H -G telegram-gateway telegram-gateway \
    && install -d -o telegram-gateway -g telegram-gateway /data

COPY --from=builder /out/telegram-gateway /usr/local/bin/telegram-gateway
COPY ee/telegram_gateway/entrypoint.sh /usr/local/bin/telegram-gateway-entrypoint
RUN chmod 0755 /usr/local/bin/telegram-gateway-entrypoint \
    && ln -s telegram-gateway /usr/local/bin/botmux

USER root
WORKDIR /data

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/api/health >/dev/null || exit 1

VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/telegram-gateway-entrypoint"]
CMD ["-addr", ":8080", "-db", "/data/botdata.db"]
