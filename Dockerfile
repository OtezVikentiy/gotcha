FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gotcha ./cmd/gotcha

FROM alpine:3.21
RUN adduser -D -u 10001 gotcha
USER gotcha
COPY --from=build /out/gotcha /usr/local/bin/gotcha
EXPOSE 8080
ENTRYPOINT ["gotcha"]
