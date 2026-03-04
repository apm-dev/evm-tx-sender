FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/tx-sender ./cmd/tx-sender

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata curl

COPY --from=builder /bin/tx-sender /bin/tx-sender

EXPOSE 8080

ENTRYPOINT ["/bin/tx-sender"]
