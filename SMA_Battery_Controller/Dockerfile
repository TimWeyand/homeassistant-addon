ARG BUILD_ARCH

# Build stage: pinned Go toolchain (must satisfy the go directive in go.mod)
FROM golang:1.27.3-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /sma_battery_controller .

# Runtime stage: Home Assistant base image (bashio) with only the binary + run.sh
FROM ghcr.io/home-assistant/${BUILD_ARCH}-base:3.24-2026.10.3

COPY --from=builder /sma_battery_controller /sma_battery_controller
COPY run.sh /
RUN chmod +x /run.sh

CMD [ "/run.sh" ]
