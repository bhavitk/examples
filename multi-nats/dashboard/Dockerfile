FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
COPY static ./static
RUN go mod tidy && go build -o /out/dashboard .

FROM alpine:3.19
RUN apk add --no-cache docker-cli docker-cli-compose
COPY --from=build /out/dashboard /usr/local/bin/dashboard
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/dashboard"]
