FROM golang:1.27.0-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/canary ./cmd/canary

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -S canary && adduser -S -G canary canary
COPY --from=build /out/canary /canary
USER canary:canary
EXPOSE 8081
ENV CANARY_HTTP_ADDR=:8081 \
    CANARY_INTERVAL=1m \
    CANARY_TIMEOUT=10s
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8081/livez"]
ENTRYPOINT ["/canary"]
