FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

ARG TARGETARCH
COPY .build/linux-$TARGETARCH/kasa_exporter /bin/kasa_exporter

EXPOSE      9498
USER        nobody
ENTRYPOINT  [ "/bin/kasa_exporter" ]
