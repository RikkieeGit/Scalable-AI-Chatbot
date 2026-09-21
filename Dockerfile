FROM golang:1.22.2-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /sys32-ai .

FROM debian:bookworm-slim

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /sys32-ai /app/sys32-ai
COPY --from=builder /src/static /app/static
COPY --from=builder /src/templates /app/templates

RUN mkdir -p /app/data

EXPOSE 8080

CMD ["/app/sys32-ai"]
