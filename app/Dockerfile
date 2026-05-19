FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN go mod tidy
RUN CGO_ENABLED=0 go build -o /out/app main.go

FROM alpine:3.20
RUN apk add --no-cache curl
COPY --from=builder /out/app /usr/local/bin/app
EXPOSE 8080
CMD ["/usr/local/bin/app"]
