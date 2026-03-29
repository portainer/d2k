FROM golang:1.26-alpine AS builder

ARG VERSION
ARG GOOS
ARG GOARCH

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${GOOS} GOARCH=${GOARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o d2k ./cmd/d2k.go

FROM scratch

COPY --from=builder /build/d2k /d2k
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENTRYPOINT ["/d2k"]
