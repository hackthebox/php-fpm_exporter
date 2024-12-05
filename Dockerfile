FROM alpine:3.20.3

COPY /php-fpm_exporter /php-fpm_exporter

EXPOSE 9253

ENTRYPOINT ["/php-fpm_exporter", "server"]
