FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY src/ ./src/

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/ud-reselling-dyndns \
    ./src

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S dyndns \
    && adduser -S -G dyndns -h /app dyndns

COPY --from=build /out/ud-reselling-dyndns /usr/local/bin/ud-reselling-dyndns

USER dyndns
WORKDIR /app

ENTRYPOINT ["/usr/local/bin/ud-reselling-dyndns"]
CMD ["-config", "/config/config.json"]
