FROM golang:1.23.11 AS builder

WORKDIR /src/goagent
COPY goagent/go.mod ./
RUN /usr/local/go/bin/go mod download
COPY goagent/ ./
RUN CGO_ENABLED=0 /usr/local/go/bin/go build -o /out/cloudlynet-agent ./cmd/agent

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl tzdata && update-ca-certificates

RUN adduser -D -H cloudlynet \
    && mkdir -p /etc/cloudlynet-agent /var/lib/cloudlynet-agent /srv/nybsys-ftp/nybsysftp/uploads \
    && chown -R cloudlynet:cloudlynet /etc/cloudlynet-agent /var/lib/cloudlynet-agent /srv/nybsys-ftp

WORKDIR /app
COPY --from=builder /out/cloudlynet-agent /usr/local/bin/cloudlynet-agent
COPY config/agent.yaml /etc/cloudlynet-agent/agent.yaml
COPY config/rules.yaml /etc/cloudlynet-agent/rules.yaml

USER cloudlynet
ENTRYPOINT ["/usr/local/bin/cloudlynet-agent"]
CMD ["--config", "/etc/cloudlynet-agent/agent.yaml"]
