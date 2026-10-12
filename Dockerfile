# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ledgerd ./cmd/ledgerd

# Static binary on a minimal image with no shell, running as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ledgerd /ledgerd
# Apache-2.0 asks redistributions to include the license and notice.
COPY LICENSE NOTICE /licenses/
LABEL org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/otyang/ledger"
ENV LEDGER_ADDR=:7710
EXPOSE 7710
USER nonroot:nonroot
ENTRYPOINT ["/ledgerd"]
