FROM golang:1.21 AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o app

FROM alpine:3.19
WORKDIR /root/
COPY --from=builder /app/app .
EXPOSE 9999
ENTRYPOINT ["./app"]