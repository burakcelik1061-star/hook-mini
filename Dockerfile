# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
 && go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
 && go build -trimpath -ldflags="-s -w" -o /out/admin ./cmd/admin

# One small runtime image holds all three binaries; compose picks which to run.
FROM alpine:3.22
RUN addgroup -S app && adduser -S -G app app
COPY --from=build /out/ /usr/local/bin/
USER app
EXPOSE 8080
CMD ["api"]
