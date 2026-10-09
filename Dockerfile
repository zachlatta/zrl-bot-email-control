# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG SOURCE_COMMIT=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/zrl-bot-email ./cmd/zrl-bot-email

FROM alpine:3.22
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 app && mkdir -p /data && chown app:app /data
COPY --from=build /out/zrl-bot-email /usr/local/bin/zrl-bot-email
USER app
ENV ADDR=0.0.0.0:8080 \
    DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["zrl-bot-email"]
CMD ["serve"]
