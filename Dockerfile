FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/registry-server ./cmd/server

FROM alpine:3.20
COPY --from=build /bin/registry-server /usr/local/bin/registry-server
EXPOSE 8080
ENTRYPOINT ["registry-server"]
