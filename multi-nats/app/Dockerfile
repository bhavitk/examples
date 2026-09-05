FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN go mod tidy && go build -o /out/client .

FROM alpine:3.19
COPY --from=build /out/client /usr/local/bin/client
ENTRYPOINT ["/usr/local/bin/client"]
