FROM golang:1.27.0-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN apk add --no-cache build-base && go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/tmpmail ./cmd/tmpmail

FROM alpine:3.22
RUN addgroup -S tmpmail && adduser -S -G tmpmail tmpmail && mkdir /data && chown tmpmail:tmpmail /data
COPY --from=build /out/tmpmail /tmpmail
USER tmpmail:tmpmail
EXPOSE 25 8080 9090
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/tmpmail", "healthcheck"]
ENTRYPOINT ["/tmpmail"]
