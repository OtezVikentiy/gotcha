# Base images are pinned by digest for reproducible builds. The tag stays for
# readability; the @sha256 is authoritative. To refresh after a base bump:
#   docker buildx imagetools inspect golang:1.26-alpine   (copy the Digest)
FROM golang:1.26-alpine@sha256:70b46548e42db77e0966aaf3619fd068734dc6c77584d526b91126504fd95816 AS build
WORKDIR /src
# Зависимости завендорены — сборка не ходит в сеть за модулями, образ собирается в
# закрытых сетях. -mod=vendor явно: при рассинхроне с go.mod сборка падает, не уходит в сеть.
COPY . .
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
RUN CGO_ENABLED=0 go build -mod=vendor \
      -ldflags "-X gitflic.ru/otezvikentiy/gotcha/internal/version.version=${VERSION} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.commit=${COMMIT} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.date=${DATE}" \
      -o /out/gotcha ./cmd/gotcha

# Кросс-бинарники агента раздаются самим инстансом (/agent/*): CGO_ENABLED=0 —
# обычный go build с GOOS/GOARCH, multi-platform buildx не нужен.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor \
      -ldflags "-s -w -X gitflic.ru/otezvikentiy/gotcha/internal/version.version=${VERSION} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.commit=${COMMIT} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.date=${DATE}" \
      -o /out/agent-dist/gotcha-agent-linux-amd64 ./cmd/gotcha-agent \
 && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor \
      -ldflags "-s -w -X gitflic.ru/otezvikentiy/gotcha/internal/version.version=${VERSION} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.commit=${COMMIT} \
                -X gitflic.ru/otezvikentiy/gotcha/internal/version.date=${DATE}" \
      -o /out/agent-dist/gotcha-agent-linux-arm64 ./cmd/gotcha-agent \
 && cd /out/agent-dist && sha256sum gotcha-agent-linux-amd64 gotcha-agent-linux-arm64 > SHA256SUMS

# Refresh with: docker buildx imagetools inspect alpine:3.21   (copy the Digest)
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d
# Каталог выгрузок (GOTCHA_EXPORT_DIR) обязан существовать и быть chown'нут ДО USER:
# именованный том docker-compose.yml иначе монтируется как root:root 0755 — Docker
# наследует владельца из образа только если каталог уже существует на этом слое.
# chmod 0700 тоже здесь: MkdirAll в main.go сужает права только каталогу, который
# создаёт сам, а этот уже существует к тому моменту.
RUN adduser -D -u 10001 gotcha \
 && mkdir -p /var/lib/gotcha/exports \
 && chown gotcha:gotcha /var/lib/gotcha/exports \
 && chmod 0700 /var/lib/gotcha/exports
USER gotcha
COPY --from=build /out/gotcha /usr/local/bin/gotcha
COPY --from=build /out/agent-dist /opt/gotcha/agent-dist
ENV GOTCHA_DIST_DIR=/opt/gotcha/agent-dist
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
