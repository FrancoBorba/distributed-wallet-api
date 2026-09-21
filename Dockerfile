# Build stage.
#
# The Go version is pinned to the one declared in go.mod, so the binary in the
# image is built by the same toolchain the tests ran under.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are copied on their own first, so a change to the source does
# not invalidate the layer that downloaded the modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off, which produces a static binary that runs on a distroless-style
# base with no libc. -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/wallet-api ./cmd/server

# Runtime stage.
#
# Nothing but the binary and the root certificates. There is no shell, no
# package manager and no source code in the final image, so a process that
# somehow gets executed here has almost nothing to work with.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 wallet

COPY --from=build /out/wallet-api /usr/local/bin/wallet-api

# The service never needs to write to its own filesystem and never needs root.
USER wallet

EXPOSE 8081

ENTRYPOINT ["/usr/local/bin/wallet-api"]
