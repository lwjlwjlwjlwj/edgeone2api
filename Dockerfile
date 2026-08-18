FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY go.mod ./
# no dependencies, go mod download is skipped

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/server ./cmd/server

FROM alpine:latest

RUN addgroup -g 1000 appuser && adduser -D -u 1000 -G appuser appuser \
    && apk add --no-cache python3

WORKDIR /app

COPY --from=builder /app/server /app/server
COPY --chown=appuser:appuser config.example.json /app/config.json
COPY --chown=appuser:appuser internal/toolcall /app/internal/toolcall

USER appuser

EXPOSE 7863

ENTRYPOINT ["/app/server"]
CMD ["--config", "/app/config.json"]