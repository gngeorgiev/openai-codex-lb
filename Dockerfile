FROM golang:1.25-alpine AS builder

WORKDIR /src
ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/codexlb ./cmd/codexlb

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/codexlb /usr/local/bin/codexlb

ENTRYPOINT ["/usr/local/bin/codexlb"]
CMD ["proxy", "--root", "/data", "--listen", "0.0.0.0:8765", "--upstream", "https://chatgpt.com/backend-api"]
