# Build stage
FROM golang:1.22-alpine AS builder

ARG VERSION=dev

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X github.com/KilimcininKorOglu/M365Bridge/pkg/models.Version=${VERSION}" -o bin/m365-bridge ./cmd/cli

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /build/bin/m365-bridge ./bin/m365-bridge

# Data directory for tokens, cache, setup.json
RUN mkdir -p data/tokens

EXPOSE 8000

ENTRYPOINT ["./bin/m365-bridge"]
CMD ["serve", "--port", "8000"]
