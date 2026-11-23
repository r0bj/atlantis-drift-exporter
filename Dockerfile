FROM golang:1.27.4 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o atlantis-drift-exporter .


FROM alpine:3.24 AS certs

RUN apk add --no-cache ca-certificates

FROM scratch

COPY --from=builder /workspace/atlantis-drift-exporter /
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

USER 65534:65534

ENTRYPOINT ["/atlantis-drift-exporter"]
