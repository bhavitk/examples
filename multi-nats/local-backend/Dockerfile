FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY *.go ./
COPY static ./static
RUN go mod tidy && CGO_ENABLED=0 go build -o /out/local-backend .

FROM alpine:3.19
COPY --from=build /out/local-backend /usr/local/bin/local-backend
ENTRYPOINT ["/usr/local/bin/local-backend"]
