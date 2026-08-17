FROM golang:1.26.6-alpine AS builder
LABEL stage=gobuilder
ENV CGO_ENABLED=0
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=dev
ARG GIT_SHA=unknown
RUN go build -trimpath \
    -ldflags="-s -w -X github.com/MERKAT0R/go-RFLink/rflink.Version=${VERSION} -X github.com/MERKAT0R/go-RFLink/rflink.GitSHA=${GIT_SHA}" \
    -o /app/go-rflink .

FROM alpine:3.24
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
