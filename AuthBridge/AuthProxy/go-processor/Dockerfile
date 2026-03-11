FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY go-processor/ ./go-processor/

RUN CGO_ENABLED=0 GOOS=linux go build -o /go-processor ./go-processor

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

COPY --from=builder /go-processor .

EXPOSE 9090

CMD ["./go-processor"]
