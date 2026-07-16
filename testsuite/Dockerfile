FROM golang:1.23.11 AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY . .
RUN CGO_ENABLED=0 /usr/local/go/bin/go build -o /out/cloudlynet-edgeagent-testsuite .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl tzdata && update-ca-certificates
WORKDIR /app
COPY --from=builder /out/cloudlynet-edgeagent-testsuite /app/cloudlynet-edgeagent-testsuite
# nanolink.conf (ST-5): identity/FTP/scenario — optional at runtime (config.go
# falls back to defaults if missing), but ship the default alongside the binary.
COPY conf ./conf
# 9000 = mock cloud; 30005 = mock device's connection-request listener.
EXPOSE 9000 30005
ENTRYPOINT ["/app/cloudlynet-edgeagent-testsuite"]
