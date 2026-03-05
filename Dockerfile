FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /agent ./cmd/

FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /agent /agent

CMD ["/agent"]
