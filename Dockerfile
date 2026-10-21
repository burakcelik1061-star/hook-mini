# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/bin/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/bin/worker ./cmd/worker \
 && go build -trimpath -ldflags="-s -w" -o /out/bin/migrate ./cmd/migrate \
 && go build -trimpath -ldflags="-s -w" -o /out/bin/admin ./cmd/admin \
 && go build -trimpath -ldflags="-s -w" -o /out/dev/receiver ./cmd/receiver

FROM alpine:3.22 AS runtime
RUN addgroup -S app && adduser -S -G app app
USER app

# The development webhook receiver (docker compose --profile demo). It has its
# own image so that the service image carries nothing production does not use.
FROM runtime AS receiver
COPY --from=build /out/dev/receiver /usr/local/bin/
EXPOSE 9000
CMD ["receiver"]

# The service image, and the default target: api, worker, migrate and admin
# in one small image; compose picks which to run.
FROM runtime AS service
COPY --from=build /out/bin/ /usr/local/bin/
EXPOSE 8080
CMD ["api"]
