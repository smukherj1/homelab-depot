FROM golang:1.25-alpine3.22 AS builder

WORKDIR /build
COPY go.mod .
COPY go.sum .
COPY minter/minter.go .

RUN go build -ldflags="-s -w" -o minter .

FROM alpine:3.22

COPY --from=builder /build/minter /minter

EXPOSE 8080

CMD ["/minter"]