FROM golang:alpine AS builder
LABEL stage=gobuilder
ENV CGO_ENABLED 0
ENV GOOS linux
RUN apk update --no-cache && apk add --no-cache tzdata
WORKDIR /build
ADD go.mod .
ADD go.sum .
RUN go mod download && go mod verify
COPY . .
RUN go build -ldflags="-s -w" -v -tags=jsoniter -o /app/go-rflink ./...
FROM alpine
RUN apk update --no-cache && apk add --no-cache ca-certificates
ENV TZ=Etc/UTC
COPY --from=builder /usr/share/zoneinfo/$TZ /usr/share/zoneinfo/$TZ
RUN ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone
WORKDIR /app
COPY --from=builder /app/go-rflink /app/go-rflink
CMD ["./go-rflink"]
