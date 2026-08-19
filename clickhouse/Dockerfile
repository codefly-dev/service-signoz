FROM docker.io/clickhouse/clickhouse-server:25.12.5@sha256:cacf32d6884291dc2ff5e0156a97f46fc53ff7c929a7906d114e268a929dfd3a

LABEL org.opencontainers.image.source="https://github.com/codefly-dev/service-signoz"

COPY config.d/signoz-cluster.xml /etc/clickhouse-server/config.d/signoz-cluster.xml
