FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY *.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -o /out/registration-api .

FROM alpine:3.19
COPY --from=build /out/registration-api /usr/local/bin/registration-api
ENTRYPOINT ["/usr/local/bin/registration-api"]
