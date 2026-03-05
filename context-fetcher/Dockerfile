FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod .
COPY main.go .

RUN go build -o server .

FROM alpine:3.20

WORKDIR /app
COPY --from=builder /app/server .

EXPOSE 8080
CMD ["./server"]
