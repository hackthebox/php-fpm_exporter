ARG GO_VERSION=1.23 \
    ALPINE_VERSION=3.21.2

FROM golang:${GO_VERSION}-alpine AS builder

ENV GOOS=linux \
    GOARCH=amd64

COPY . /app

WORKDIR /app

RUN go mod download

RUN go build -o php-fpm-exporter .

FROM alpine:${ALPINE_VERSION}

COPY --from=builder /app/php-fpm-exporter .

EXPOSE 9253

ENTRYPOINT [ "/php-fpm-exporter", "server" ]
