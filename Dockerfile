FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG CMD

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

    
FROM alpine:3.20
WORKDIR /app

RUN apk add --no-cache ca-certificates

COPY --from=build /out/app /app/app

RUN adduser -D -H appuser && chown -R appuser:appuser /app
USER appuser

ENTRYPOINT ["/app/app"]