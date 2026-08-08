FROM golang:1.26.5-alpine AS builder
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
    -ldflags="-s -w -X rflink.Version=${VERSION} -X rflink.GitSHA=${GIT_SHA}" \
    -o /app/go-rflink .
 
FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Etc/UTC
WORKDIR /app
COPY --from=builder /app/go-rflink /app/go-rflink
# serial device is expected to be passed at runtime (--device)
ENTRYPOINT ["./go-rflink"]