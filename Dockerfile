# Base images are pinned by digest for reproducible builds. The tag stays for
# readability; the @sha256 is authoritative. To refresh after a base bump:
#   docker buildx imagetools inspect golang:1.26-alpine   (copy the Digest)
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
RUN CGO_ENABLED=0 go build \
      -ldflags "-X gitflic.ru/otezvikentiy/gotcha/internal/version.version=${VERSION} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.commit=${COMMIT} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.date=${DATE}" \
      -o /out/gotcha ./cmd/gotcha

# Refresh with: docker buildx imagetools inspect alpine:3.21   (copy the Digest)
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d
RUN adduser -D -u 10001 gotcha
USER gotcha
COPY --from=build /out/gotcha /usr/local/bin/gotcha
EXPOSE 8080
ENTRYPOINT ["gotcha"]
