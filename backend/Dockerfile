# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/simulator ./cmd/simulator \
 && go build -trimpath -ldflags="-s -w" -o /out/verify ./cmd/verify

# distroless: no shell, no package manager, runs as uid 65532
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/api"]
