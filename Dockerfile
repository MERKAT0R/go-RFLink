FROM golang:alpine

WORKDIR /go/src/github.com/MERKAT0R/go-rflink
COPY . .

RUN go install -v ./...

CMD ["go-rflink"]
