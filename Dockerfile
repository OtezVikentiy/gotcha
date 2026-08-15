# Base images are pinned by digest for reproducible builds. The tag stays for
# readability; the @sha256 is authoritative. To refresh after a base bump:
#   docker buildx imagetools inspect golang:1.26-alpine   (copy the Digest)
FROM golang:1.26-alpine@sha256:70b46548e42db77e0966aaf3619fd068734dc6c77584d526b91126504fd95816 AS build
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

# Кросс-бинарники агента раздаются самим инстансом (/agent/*, спека A2 §3.1):
# CGO_ENABLED=0 — обычный go build с GOOS/GOARCH, multi-platform buildx не нужен.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -ldflags "-s -w -X gitflic.ru/otezvikentiy/gotcha/internal/version.version=${VERSION} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.commit=${COMMIT} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.date=${DATE}" \
      -o /out/agent-dist/gotcha-agent-linux-amd64 ./cmd/gotcha-agent \
 && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
      -ldflags "-s -w -X gitflic.ru/otezvikentiy/gotcha/internal/version.version=${VERSION} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.commit=${COMMIT} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.date=${DATE}" \
      -o /out/agent-dist/gotcha-agent-linux-arm64 ./cmd/gotcha-agent \
 && cd /out/agent-dist && sha256sum gotcha-agent-linux-amd64 gotcha-agent-linux-arm64 > SHA256SUMS

# Refresh with: docker buildx imagetools inspect alpine:3.21   (copy the Digest)
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d
RUN adduser -D -u 10001 gotcha
USER gotcha
COPY --from=build /out/gotcha /usr/local/bin/gotcha
COPY --from=build /out/agent-dist /opt/gotcha/agent-dist
ENV GOTCHA_AGENT_DIST_DIR=/opt/gotcha/agent-dist
EXPOSE 8080
# Проверка состояния — подкомандой самого бинаря, а не curl/wget: тогда она
# зависит только от того, что в образе точно есть. Спрашивает /readyz, то есть
# «готов ли работать», а не «жив ли процесс».
#
# start-period покрывает первый старт: миграции держат порт закрытым до минуты,
# и без него контейнер успевал бы стать unhealthy ещё до того, как начал
# отвечать.
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 \
    CMD ["gotcha", "--healthcheck"]
ENTRYPOINT ["gotcha"]
