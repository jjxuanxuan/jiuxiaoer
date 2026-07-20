FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -tags=nomsgpack -p=1 -trimpath -ldflags="-s -w" -o /out/jiuxiaoer-api ./cmd/api

FROM alpine:3.23

RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=build /out/jiuxiaoer-api /app/jiuxiaoer-api
USER app
EXPOSE 8080
ENTRYPOINT ["/app/jiuxiaoer-api"]
