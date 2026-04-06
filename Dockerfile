FROM golang:1.24-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app

# Cache dependency downloads separately from source
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -tags "fts5" -ldflags="-w -s" -o memebot .

# ──────────────────────────────────────────────
FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata sqlite

WORKDIR /app
COPY --from=builder /app/memebot .

CMD ["./memebot"]
