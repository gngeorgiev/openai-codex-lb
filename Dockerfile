FROM golang:1.25-alpine AS builder

WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN set -eux; \
	GOARM=""; \
	if [ "${TARGETARCH:-}" = "arm" ]; then \
		case "${TARGETVARIANT:-}" in \
			v6) GOARM=6 ;; \
			v7|"") GOARM=7 ;; \
			*) echo "unsupported arm variant: ${TARGETVARIANT}" >&2; exit 1 ;; \
		esac; \
	fi; \
	CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} GOARM="${GOARM}" go build -trimpath -ldflags="-s -w" -o /out/codexlb ./cmd/codexlb

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/codexlb /usr/local/bin/codexlb

ENTRYPOINT ["/usr/local/bin/codexlb"]
CMD ["proxy", "--root", "/data", "--listen", "0.0.0.0:8765", "--upstream", "https://chatgpt.com/backend-api"]
