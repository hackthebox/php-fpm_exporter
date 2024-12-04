FROM golang:1.22-alpine AS builder

ENV GO111MODULE=on
ENV GOPATH=/go
WORKDIR /go/src/app

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN GOOS=linux GOARCH=amd64 go build -o /app/php-fpm_exporter .

FROM alpine:3.20.3

COPY --from=builder /app/php-fpm_exporter /php-fpm_exporter

EXPOSE 9253

ENTRYPOINT ["/php-fpm_exporter", "server"]
