FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -tags=nomsgpack -p=1 -trimpath -ldflags="-s -w" -o /out/jiuxiaoer-api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -tags=nomsgpack -p=1 -trimpath -ldflags="-s -w" -o /out/jiuxiaoer-worker ./cmd/worker

FROM alpine:3.23

RUN apk add --no-cache tzdata \
    && addgroup -S app \
    && adduser -S -G app app
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=build /out/jiuxiaoer-api /app/jiuxiaoer-api
COPY --from=build /out/jiuxiaoer-worker /app/jiuxiaoer-worker
USER app
EXPOSE 8080
ENTRYPOINT ["/app/jiuxiaoer-api"]
