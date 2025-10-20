FROM golang:1.25-alpine3.22 AS builder

WORKDIR /build
COPY go.mod .
COPY minter/minter.go .

RUN go build -o minter .

FROM alpine:3.22

COPY --from=builder /build/minter /minter

EXPOSE 8080

CMD ["/minter"]