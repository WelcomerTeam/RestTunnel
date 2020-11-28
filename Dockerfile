FROM golang:1.15-alpine AS build_base

RUN apk add --no-cache git

WORKDIR /tmp/resttunnel

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o ./out/resttunnel ./cmd/main.go

FROM alpine:3.9
RUN apk add ca-certificates

COPY --from=build_base /tmp/resttunnel/out/resttunnel /app/resttunnel

EXPOSE 8000
CMD ["/app/resttunnel"]