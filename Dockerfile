FROM golang:1.25-alpine AS build
# Networks that cannot reach proxy.golang.org can override:
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct .
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/cmdcode2api ./cmd/cmdcode2api

FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/cmdcode2api /usr/local/bin/cmdcode2api

# config.yaml and usage.json live in /data — mount a volume there so both
# survive container replacement.
WORKDIR /data
VOLUME /data
EXPOSE 11434

# --host 0.0.0.0 makes the gateway reachable from outside the container;
# flags override config.yaml, so no config edit is needed.
ENTRYPOINT ["cmdcode2api"]
CMD ["--host", "0.0.0.0"]
