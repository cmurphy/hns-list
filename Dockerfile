FROM golang:1.19-alpine AS builder
WORKDIR /go/src/github.com/cmurphy/hns-list/server
COPY ./server .
RUN go build -o hns-list-server main.go

FROM alpine:latest
WORKDIR /
COPY --from=builder /go/src/github.com/cmurphy/hns-list/server ./
CMD ["./hns-list-server"]
