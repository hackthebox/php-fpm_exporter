ARG GO_VERSION=1.13 \
    ALPINE_VERSION=3.21.2 \
    GOOS=linux \
    GOARCH=amd64

FROM golang:${GO_VERSION}-alpine AS builder

COPY . /app

WORKDIR /app

RUN go mod tidy

RUN go build -o php-fpm-exporter .

FROM alpine:${ALPINE_VERSION}

COPY php-fpm_exporter /

COPY --from=builder /app/php-fpm-exporter /

EXPOSE 9253

ENTRYPOINT [ "/php-fpm-exporter", "server" ]
