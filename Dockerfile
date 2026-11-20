# One image for every role: api, worker, migrate, seed, user.
# Build:  docker build -t core-jobs-service .
FROM golang:1.26.11-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags='-s -w' -o /out/app ./cmd/app

# Distroless: no shell or package manager; runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
COPY --from=build /src/configs/seed /configs/seed
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/app"]
CMD ["api"]
