FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/bifrost-scheduler ./cmd/bifrost-scheduler

FROM alpine:3.22
RUN addgroup -S scheduler && adduser -S scheduler -G scheduler
USER scheduler
WORKDIR /app
COPY --from=build /out/bifrost-scheduler /usr/local/bin/bifrost-scheduler
COPY config.example.json /app/config.example.json
ENTRYPOINT ["bifrost-scheduler"]
CMD ["daemon"]
