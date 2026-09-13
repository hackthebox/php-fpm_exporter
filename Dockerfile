FROM golang:1.26.8-alpine AS builder

ENV GOOS=linux \
    GOARCH=amd64

COPY . /app

WORKDIR /app

RUN go mod download

RUN go build -o php-fpm-exporter .

FROM alpine:3.24.1

COPY --from=builder /app/php-fpm-exporter .

EXPOSE 9253

ENTRYPOINT [ "/php-fpm-exporter", "server" ]
