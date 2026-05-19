FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN go mod tidy
RUN CGO_ENABLED=0 go build -o /out/worker main.go

FROM alpine:3.20
COPY --from=builder /out/worker /usr/local/bin/worker
EXPOSE 8090
CMD ["/usr/local/bin/worker"]
