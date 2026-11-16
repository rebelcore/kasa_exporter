FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

ARG TARGETARCH
COPY .build/linux-$TARGETARCH/kasa_exporter /bin/kasa_exporter

EXPOSE      9498
USER        nobody
ENTRYPOINT  [ "/bin/kasa_exporter" ]
