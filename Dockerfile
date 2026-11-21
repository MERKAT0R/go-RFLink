FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder
LABEL stage=gobuilder
ENV CGO_ENABLED=0
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=0.4.0
ARG GIT_SHA=unknown
RUN go build -trimpath \
    -ldflags="-s -w -X github.com/MERKAT0R/go-rflink/rflink.Version=${VERSION} -X github.com/MERKAT0R/go-rflink/rflink.GitSHA=${GIT_SHA}" \
    -o /app/go-rflink .

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
# ca-certificates: TLS to MQTT brokers
# tzdata: correct timestamps in logs / runtime
# wget: Docker HEALTHCHECK / compose healthcheck (busybox wget exists, full package is explicit)
RUN apk add --no-cache ca-certificates tzdata wget
ENV TZ=Etc/UTC
WORKDIR /app
COPY --from=builder /app/go-rflink /app/go-rflink
# serial device is expected to be passed at runtime (--device)
# optional: HEALTHCHECK when HTTP is enabled (override via compose if Listen differs)
ENTRYPOINT ["./go-rflink"]
