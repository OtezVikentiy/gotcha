# Установка без Docker (bare-metal)

Альтернатива [Docker-установке](/docs/installation): Gotcha, PostgreSQL и ClickHouse ставятся системными пакетами прямо на хост и запускаются под systemd. Инструкция рассчитана на тот же уровень подготовки, что и Docker-путь — Linux-сервер по SSH, без опыта администрирования баз данных.

**Граница поддержки.** Этот путь поддерживается наравне с Docker — при условии, что вы не правили руками ни сам скрипт, ни созданные им конфиги (юнит systemd, `/etc/gotcha/gotcha.env`, конфиги PostgreSQL/ClickHouse). Флаги, с которыми поддержка сохраняется в полном объёме: `--domain`, `--email`, `--no-proxy`, `--no-firewall`, `--mem-limit`, `--base-url`, `--download-base`, `--version`, `--from-tarball`. Отдельный случай — `--skip-databases` с собственными PostgreSQL/ClickHouse: раз базы не наши, мы можем помочь диагностировать проблему, но не гарантировать её решение — ответственность за версии, конфигурацию и доступность этих баз на вас.

Ставится скриптом `install-bare-metal.sh` (раздел «Установка скриптом» ниже) либо теми же командами руками (раздел «Ручная установка») — это не приложение к инструкции, а полноценный путь: скрипт лишь автоматизирует его.

## Что понадобится

- **Linux-сервер.** Заявлены ровно те дистрибутивы и архитектуры, на которых установка прогоняется в CI на каждый релиз, — таблица ниже. Дистрибутивы, унаследованные от заявленных (`ID_LIKE` содержит `debian`, `ubuntu`, `rhel` или `fedora`), скрипт тоже принимает, и они, скорее всего, работают, — но мы их не гоняем и потому не заявляем. **AlmaLinux/Rocky/RHEL 8 не поддерживается** — преflight останавливается явной ошибкой, нужны 9 или 10.
- **Архитектура** amd64 или arm64.
- **systemd** — практически любой современный сервер уже под ним; проверить: `[ -d /run/systemd/system ] && echo ok`.
- **root-доступ** по SSH — установщик пишет в `/etc`, `/opt`, `/usr/local/bin`, `/var/lib` и ставит системные пакеты.
- (Не обязательно, но желательно для реального использования) доменное имя, указывающее на IP сервера — понадобится для TLS-сертификата.

Дистрибутивы и архитектуры, которые прогоняются в CI на каждый релиз:

| ОС | Архитектуры |
|---|---|
| AlmaLinux 9 | amd64, arm64 |
| RHEL 9 | amd64, arm64 |
| AlmaLinux 10 | amd64, arm64 |
| RHEL 10 | amd64, arm64 |
| Rocky Linux 9 | amd64, arm64 |
| Rocky Linux 10 | amd64, arm64 |
| Debian 12 | amd64 |
| Debian 13 | amd64 |
| Ubuntu 24.04 | amd64, arm64 |
| Ubuntu 26.04 | amd64 |

## Системные требования

|      | Минимум | Рекомендуется |
|------|---------|---------------|
| CPU  | 2 vCPU  | 4 vCPU        |
| RAM  | 2 ГБ    | 4 ГБ и больше |
| Диск | 20 ГБ SSD | 40 ГБ SSD и больше |

Требования такие же, как у Docker-пути, и по той же причине: основной потребитель ресурсов — ClickHouse, а не способ поставки. Преflight-проверка скрипта принимает от 1900 МБ RAM (не 2048) — у части облачных «2 ГБ»-тарифов часть памяти уходит на прошивку/гипервизор ещё до старта ОС, и ровно 2048 МБ `MemTotal` они не показывают никогда.

Пик потребления памяти на этом пути измерен отдельно от Docker (переносить докерный замер без проверки было бы нечестно): в прогонах приёмочных тестов инсталлятора все три процесса (PostgreSQL, ClickHouse, приложение) суммарно занимали 886–982 МБ — верхнюю границу этого диапазона и стоит держать в уме как консервативную оценку. Замер сделан в контейнере на машине разработчика, а не на CI-раннере; при первом же прогоне на CI с другим пиком цифру здесь нужно будет поправить. Даже верхняя граница сопоставима с докерным замером 1006 МБ на релизе 1.6.1 — минимум 2 ГБ подтверждён на обоих путях поставки.

Версии, которые ставит этот путь, зафиксированы в скрипте и совпадают с Docker-поставкой: PostgreSQL 17, ClickHouse 25.3, само приложение собрано с Go 1.26.

## Ручная установка

Каждый шаг — то же самое, что делает скрипт `install-bare-metal.sh`, командами напрямую. Полезно, если политика хоста не допускает сторонних скриптов от root, или если что-то в автоматической установке пошло не так и нужно доделать руками.

### 1. Подготовьте хост

```bash
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y curl tar gnupg openssl coreutils sudo iproute2
```

`sudo` и `iproute2` (команда `ss`) нужны и дальше по шагам, и скрипту: без первого не
создать роль в PostgreSQL от пользователя `postgres`, без второго нечем проверить порты.
На минимальном образе Debian/Ubuntu ни того, ни другого нет — преflight скрипта
отказывает с кодом 3 и называет недостающий пакет.

Проверьте порты, которые понадобятся: 8080 (приложение), 80 (если ставите nginx), 5432/8123/9000 (если ставите PostgreSQL/ClickHouse этим же способом).

```bash
ss -ltn | grep -E ':(8080|80|5432|8123|9000)\b'
```

Если что-то уже слушает эти порты и это не сама СУБД, которую вы устанавливаете, — освободите порт или уберите его из списка нужных (например, `--skip-databases`/`--no-proxy` у скрипта).

### 2. Установите PostgreSQL 17

Дальше команды делятся по семейству дистрибутива — репозиторий, пакетный менеджер и
пути к данным разные, поведение приложения от этого не зависит.

**На Debian/Ubuntu:**

Определите кодовое имя дистрибутива и подключите официальный репозиторий PGDG:

```bash
CODENAME=$(. /etc/os-release && printf '%s\n' "$VERSION_CODENAME")
curl -fsSL -o /tmp/pgdg.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc
gpg --dearmor </tmp/pgdg.asc >/usr/share/keyrings/gotcha-pgdg.gpg
KEY=/usr/share/keyrings/gotcha-pgdg.gpg
REPO=https://apt.postgresql.org/pub/repos/apt
printf 'deb [signed-by=%s] %s %s-pgdg main\n' "$KEY" "$REPO" "$CODENAME" \
  >/etc/apt/sources.list.d/gotcha-pgdg.list
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y postgresql-17
```

Отпечаток ключа PGDG (apt) — `B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8`; сверьте его перед импортом (`gpg --with-colons --import-options show-only --import /tmp/pgdg.asc`), не доверяя загрузке вслепую.

Если PGDG ещё не опубликовал пакеты для вашего кодового имени (бывает с самыми свежими релизами дистрибутива), проверьте, что штатный репозиторий дистрибутива сам даёт именно 17-й мажор (`apt-cache policy postgresql`), и ставьте `postgresql` без PGDG. Если штатный мажор другой — придётся либо подождать PGDG, либо поставить PostgreSQL 17 из стороннего источника самостоятельно.

Настройте параметры, важные под нагрузку ClickHouse-соседа на том же диске:

```bash
CONF_DIR=$(find /etc/postgresql -mindepth 2 -maxdepth 2 -type d -name main | head -n1)
mkdir -p "$CONF_DIR/conf.d"
cat >"$CONF_DIR/conf.d/10-gotcha.conf" <<'EOF'
random_page_cost = 1.1
effective_io_concurrency = 200
EOF
systemctl restart postgresql
```

Создайте роль и базу:

```bash
sudo -u postgres psql -c "CREATE ROLE gotcha LOGIN PASSWORD 'придумайте-свой-пароль'"
sudo -u postgres psql -c "CREATE DATABASE gotcha OWNER gotcha"
```

**На AlmaLinux/Rocky/RHEL 9 и 10:**

Подключите официальный репозиторий PGDG (ключ для rpm — другой, чем для apt) и
поставьте пакет вместе с `contrib`: расширение `citext`, нужное миграциям, на EL
живёт отдельным пакетом, а не внутри `-server`, как на Debian/Ubuntu.

```bash
EL_MAJOR=$(. /etc/os-release && printf '%s\n' "${VERSION_ID%%.*}")
curl -fsSL -o /tmp/pgdg.asc https://download.postgresql.org/pub/repos/yum/keys/PGDG-RPM-GPG-KEY-RHEL
mkdir -p /etc/pki/rpm-gpg
cp /tmp/pgdg.asc /etc/pki/rpm-gpg/gotcha-pgdg.asc
rpm --import /etc/pki/rpm-gpg/gotcha-pgdg.asc
cat >/etc/yum.repos.d/gotcha-pgdg.repo <<EOF
[pgdg-common]
name=PostgreSQL common RPMs for RHEL \$releasever - \$basearch
baseurl=https://download.postgresql.org/pub/repos/yum/common/redhat/rhel-$EL_MAJOR-\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/gotcha-pgdg.asc

[pgdg17]
name=PostgreSQL 17 for RHEL \$releasever - \$basearch
baseurl=https://download.postgresql.org/pub/repos/yum/17/redhat/rhel-$EL_MAJOR-\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/gotcha-pgdg.asc
EOF
[ "$EL_MAJOR" = 9 ] && dnf -y module disable postgresql
dnf install -y postgresql17-server postgresql17-contrib
```

Отпечаток ключа PGDG (rpm) — `D4BF08AE67A0B4C7A1DBCCD240BCA2B408B40D20`, другой, чем у
apt-ключа выше; сверьте тем же способом. На EL9 штатный модуль дистрибутива
`postgresql` конфликтует с пакетом PGDG и отключается заранее; на EL10 такого
модуля нет, и команда просто ничего не делает.

Инициализируйте кластер и настройте те же параметры — `postgresql.conf.sample`,
из которого `initdb` копирует конфиг, несёт `include_dir` закомментированной, без
явной дописки дропин ниже не подхватится вообще:

```bash
/usr/pgsql-17/bin/postgresql-17-setup initdb
mkdir -p /var/lib/pgsql/17/data/conf.d
cat >/var/lib/pgsql/17/data/conf.d/10-gotcha.conf <<'EOF'
random_page_cost = 1.1
effective_io_concurrency = 200
EOF
printf '%s\ninclude_dir = %s\n' '# gotcha: conf.d include' "'conf.d'" \
  >>/var/lib/pgsql/17/data/postgresql.conf
awk '$1=="host" && $4=="127.0.0.1/32" && ($5=="scram-sha-256" || $5=="md5"){f=1} END{exit f?0:1}' \
  /var/lib/pgsql/17/data/pg_hba.conf \
  || printf '%s\nhost all all 127.0.0.1/32 scram-sha-256\n' '# gotcha: conf.d include' \
       >>/var/lib/pgsql/17/data/pg_hba.conf
systemctl enable --now postgresql-17
```

Создайте роль и базу — `psql` из пакета PGDG не лежит в `PATH`:

```bash
sudo -u postgres /usr/pgsql-17/bin/psql \
  -c "CREATE ROLE gotcha LOGIN PASSWORD 'придумайте-свой-пароль'"
sudo -u postgres /usr/pgsql-17/bin/psql -c "CREATE DATABASE gotcha OWNER gotcha"
```

DSN для шага 6 — один и тот же на обоих семействах: `postgres://gotcha:<пароль>@127.0.0.1:5432/gotcha?sslmode=disable`.

### 3. Установите ClickHouse 25.3

**На Debian/Ubuntu:**

Подключите репозиторий ClickHouse:

```bash
curl -fsSL -o /tmp/clickhouse.asc https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key
gpg --dearmor </tmp/clickhouse.asc >/usr/share/keyrings/gotcha-clickhouse.gpg
KEY=/usr/share/keyrings/gotcha-clickhouse.gpg
REPO=https://packages.clickhouse.com/deb
printf 'deb [signed-by=%s] %s stable main\n' "$KEY" "$REPO" \
  >/etc/apt/sources.list.d/gotcha-clickhouse.list
apt-get update
```

Отпечаток ключа ClickHouse — `3A9EA1193A97B548BE1457D48919F6BD2B48D754`, сверяйте тем же способом, что и для PGDG.

ClickHouse не публикует пакет без точного патча в номере версии — найдите патч для нужного мажора.минора и поставьте пакеты этой версией, все три сразу (иначе apt подтянет `clickhouse-common-static` последним мажором и упрётся в конфликт зависимостей):

```bash
CH_PKG_VERSION=$(apt-cache madison clickhouse-server \
  | awk -F'|' '{gsub(/^[ \t]+|[ \t]+$/,"",$2)} $2~/^25\.3\./{print $2; exit}')
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  "clickhouse-server=$CH_PKG_VERSION" \
  "clickhouse-client=$CH_PKG_VERSION" \
  "clickhouse-common-static=$CH_PKG_VERSION"
```

`DEBIAN_FRONTEND=noninteractive` здесь не ради тишины в логе: постинстал `clickhouse-server` на живом терминале спрашивает пароль для пользователя `default`, и любой заданный там пароль ломает создание базы двумя шагами ниже. Скрипт ставит пакеты так же.

Скопируйте тюнинг-конфиги из тарбола релиза (тот же архив, откуда взят бинарь на шаге 5):

```bash
mkdir -p /etc/clickhouse-server/config.d
cp clickhouse/00-common.xml /etc/clickhouse-server/config.d/00-common.xml
```

На хосте с RAM < 4 ГБ добавьте ещё и оверлей для слабого железа (тот же принцип, что `docker-compose.small.yml` в Docker-пути):

```bash
cp clickhouse/10-small.xml /etc/clickhouse-server/config.d/10-small.xml
```

Заведите пользователя `gotcha` с паролем (ClickHouse хранит SHA-256 хеш, не сам пароль):

```bash
CH_PASSWORD=$(openssl rand -hex 24)
CH_PASSWORD_HASH=$(printf '%s' "$CH_PASSWORD" | sha256sum | awk '{print $1}')
mkdir -p /etc/clickhouse-server/users.d
cat >/etc/clickhouse-server/users.d/10-gotcha.xml <<EOF
<clickhouse>
    <users>
        <gotcha>
            <password_sha256_hex>$CH_PASSWORD_HASH</password_sha256_hex>
            <networks>
                <ip>::1</ip>
                <ip>127.0.0.1</ip>
            </networks>
            <profile>default</profile>
            <quota>default</quota>
            <default_database>gotcha</default_database>
            <access_management>0</access_management>
        </gotcha>
    </users>
</clickhouse>
EOF
printf 'ClickHouse, пароль пользователя gotcha: %s\n' "$CH_PASSWORD"
```

Запишите напечатанный пароль: в конфиге ClickHouse лежит только SHA-256, исходный пароль из него не восстанавливается, а на шаге 6 он нужен в DSN. Потеряли — не страшно, но придётся выпускать новый (рецепт в «Типичных ошибках» ниже).

Поднимите лимит открытых файлов (заводской слишком мал под нагрузку ClickHouse) и запустите:

```bash
mkdir -p /etc/systemd/system/clickhouse-server.service.d
cat >/etc/systemd/system/clickhouse-server.service.d/override.conf <<'EOF'
[Service]
LimitNOFILE=262144
EOF
systemctl daemon-reload
systemctl enable --now clickhouse-server
```

Дождитесь готовности и создайте базу:

```bash
until curl -fsS -o /dev/null http://127.0.0.1:8123/ping; do sleep 1; done
clickhouse-client --query "CREATE DATABASE IF NOT EXISTS gotcha"
```

**На AlmaLinux/Rocky/RHEL 9 и 10:**

Подключите репозиторий тем же способом, что и для PGDG: скачать ключ, сверить
отпечаток, импортировать через `rpm`, отрендерить `.repo`-файл.

```bash
curl -fsSL -o /tmp/clickhouse.asc https://packages.clickhouse.com/rpm/stable/repodata/repomd.xml.key
mkdir -p /etc/pki/rpm-gpg
cp /tmp/clickhouse.asc /etc/pki/rpm-gpg/gotcha-clickhouse.asc
rpm --import /etc/pki/rpm-gpg/gotcha-clickhouse.asc
cat >/etc/yum.repos.d/gotcha-clickhouse.repo <<EOF
[gotcha-clickhouse]
name=ClickHouse
baseurl=https://packages.clickhouse.com/rpm/stable/
enabled=1
gpgcheck=0
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/gotcha-clickhouse.asc
EOF
```

Отпечаток ключа ClickHouse (rpm) — тот же `3A9EA1193A97B548BE1457D48919F6BD2B48D754`,
что и у apt-репозитория; сверяйте тем же способом. `gpgcheck=0` здесь не
ослабление: пакеты ClickHouse не подписаны индивидуально ни у нас, ни в их
собственном `clickhouse.repo` — доверие даёт `repo_gpgcheck=1` (метаданные
подписаны сверенным ключом и несут SHA-256 каждого пакета).

ClickHouse не публикует пакет без точного патча в номере версии — вывод `dnf` со
списком версий переносит длинные строки, так что версия может оказаться
отдельным полем на следующей строке:

```bash
CH_PKG_VERSION=$(dnf -qy --showduplicates list clickhouse-server 2>/dev/null | awk -v v=25.3. '
  function is_arch(s) { return s ~ /\.(noarch|x86_64|aarch64)$/ }
  function has_prefix(s) { return substr(s, 1, length(v)) == v }
  NF >= 2 && is_arch($1) { if (has_prefix($2)) print $2; next }
  NF >= 1 && !is_arch($1) { if (has_prefix($1)) print $1 }
' | sort -V | tail -n1)
dnf install -y \
  "clickhouse-server-$CH_PKG_VERSION" \
  "clickhouse-client-$CH_PKG_VERSION" \
  "clickhouse-common-static-$CH_PKG_VERSION"
```

Дальше — то же самое, что и на Debian/Ubuntu выше, начиная с копирования
тюнинг-конфигов из тарбола: пути `/etc/clickhouse-server/config.d`,
`/etc/clickhouse-server/users.d`, systemd-override и проверка готовности по
`/ping` одинаковы на обоих семействах.

DSN для шага 6: `clickhouse://gotcha:<пароль>@127.0.0.1:9000/gotcha`.

### 4. Создайте системного пользователя приложения

```bash
useradd --system --no-create-home --shell /usr/sbin/nologin gotcha
```

Без домашнего каталога и без интерактивного шелла — процесс не заходит под этим пользователем, только запускается им.

### 5. Скачайте и установите бинарь

Возьмите тарбол релиза с GitHub (замените `X.Y.Z` и `<arch>` на amd64 или arm64):

```bash
URL="https://github.com/OtezVikentiy/gotcha/releases/download/vX.Y.Z"
TARBALL="gotcha-X.Y.Z-linux-<arch>.tar.gz"
curl -fsSL -o "$TARBALL" "$URL/$TARBALL"
curl -fsSL -o SHA256SUMS.txt "$URL/SHA256SUMS.txt"
grep " $TARBALL\$" SHA256SUMS.txt | sha256sum -c -
tar xzf "$TARBALL"
cd gotcha-X.Y.Z-linux-<arch>
```

Установите бинарь сервера и раздачу бинарей агента (последнее — то, что отдаёт `/agent/gotcha-agent-linux-amd64` при подключении хостов, см. [Хосты](/docs/hosts)):

```bash
install -m 0755 -o root -g root gotcha /usr/local/bin/gotcha
mkdir -p /opt/gotcha/agent-dist
cp -a agent-dist/. /opt/gotcha/agent-dist/
```

### 6. Заведите файл окружения

```bash
mkdir -p /etc/gotcha
GOTCHA_SECRET=$(openssl rand -base64 48)
cat >/etc/gotcha/gotcha.env <<EOF
GOTCHA_PG_DSN=postgres://gotcha:<пароль-из-шага-2>@127.0.0.1:5432/gotcha?sslmode=disable
GOTCHA_CH_DSN=clickhouse://gotcha:<пароль-из-шага-3>@127.0.0.1:9000/gotcha
GOTCHA_SECRET_KEY=$GOTCHA_SECRET
GOTCHA_BASE_URL=https://gotcha.example.com
GOTCHA_DIST_DIR=/opt/gotcha/agent-dist
GOMEMLIMIT=819MiB
GOTCHA_LISTEN_ADDR=127.0.0.1:8080
EOF
chown root:gotcha /etc/gotcha/gotcha.env
chmod 0640 /etc/gotcha/gotcha.env
```

Если шаг 3 выполнялся в этом же сеансе шелла, вместо `<пароль-из-шага-3>` можно подставить `$CH_PASSWORD` — переменная ещё жива, и heredoc её раскроет.

Про `GOTCHA_BASE_URL` см. предупреждение о 403 в разделе «Типичные ошибки» ниже — задайте его сразу верным адресом, включая схему. Про `GOTCHA_LISTEN_ADDR=127.0.0.1:8080` — то же, что loopback-бинд у Docker-пути: без обратного прокси порт наружу не торчит. `GOMEMLIMIT=819MiB` — 80% от `MemoryMax=1024M` из юнита ниже; если меняете лимит памяти, пересчитайте оба значения синхронно (`--mem-limit` у скрипта делает это за вас).

Файл хранит секреты (мастер-ключ шифрования, пароли обеих баз) — права `0640` и владелец `root:gotcha` обязательны, как и для `.env` в Docker-пути (см. [Резервное копирование](/docs/backup-restore)).

### 7. Установите systemd-юнит

```bash
cat >/etc/systemd/system/gotcha.service <<'EOF'
[Unit]
Description=gotcha monitoring server
After=postgresql.service postgresql-17.service clickhouse-server.service network-online.target

[Service]
Type=simple
User=gotcha
Group=gotcha
EnvironmentFile=/etc/gotcha/gotcha.env
ExecStart=/usr/local/bin/gotcha
Restart=always
RestartSec=5
TimeoutStopSec=90

StateDirectory=gotcha
StateDirectoryMode=0700

ProtectSystem=strict
PrivateTmp=yes
ProtectHome=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
SystemCallFilter=@system-service
SystemCallArchitectures=native
UMask=0077
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
MemoryDenyWriteExecute=yes

TasksMax=512
MemoryAccounting=yes
MemoryMax=1024M

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
```

`After=` без `Requires=` — намеренно: перезапуск базы не должен тянуть за собой перезапуск приложения, оно переживает временную недоступность базы (см. `/readyz` ниже) и само восстановится через `Restart=always`, когда база вернётся.

### 8. Прогоните миграции и запустите приложение

```bash
systemd-run --pipe --wait --collect --uid=gotcha --gid=gotcha \
  --property="EnvironmentFile=/etc/gotcha/gotcha.env" \
  /usr/local/bin/gotcha --migrate-only
systemctl enable --now gotcha
```

Дождитесь, пока пройдёт healthcheck (см. «Самопроверка» ниже), или следите за логом:

```bash
journalctl -u gotcha -f
```

### 9. Разверните nginx (если нужен внешний доступ)

Пропустите этот шаг, если публикуете инстанс за уже существующим прокси или обращаетесь к нему только через SSH-туннель на `127.0.0.1:8080`.

**На Debian/Ubuntu:**

```bash
DEBIAN_FRONTEND=noninteractive apt-get install -y nginx
rm -f /etc/nginx/sites-enabled/default
cat >/etc/nginx/sites-available/gotcha <<'EOF'
# gotcha site: install-bare-metal.sh keeps local edits below on re-run
server {
    listen 80;
    server_name gotcha.example.com;
    client_max_body_size 64m;

    location ~ ^/(metrics|version)$ {
        allow 127.0.0.1;
        allow ::1;
        deny all;
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
EOF
ln -sf ../sites-available/gotcha /etc/nginx/sites-enabled/gotcha
nginx -t
systemctl enable --now nginx
systemctl reload nginx
```

**На AlmaLinux/Rocky/RHEL 9 и 10:** путь к сайту и пакетный менеджер другие, сам
конфиг — тот же; отдельного каталога `sites-enabled` и симлинка на EL нет —
`nginx.conf` штатно подключает всё из `conf.d`.

```bash
dnf install -y nginx
cat >/etc/nginx/conf.d/gotcha.conf <<'EOF'
# gotcha site: install-bare-metal.sh keeps local edits below on re-run
server {
    listen 80;
    server_name gotcha.example.com;
    client_max_body_size 64m;

    location ~ ^/(metrics|version)$ {
        allow 127.0.0.1;
        allow ::1;
        deny all;
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
EOF
nginx -t
systemctl enable --now nginx
systemctl reload nginx
```

Первая строка файла — метка для скрипта: увидев её, повторный запуск
`install-bare-metal.sh` не перерисовывает сайт, а оставляет как есть, пока в нём
стоит тот же `server_name`. Именно так переживает обновление TLS-блок, который
дописывает certbot на шаге 11. Если сайт нужно перерисовать с нуля — удалите или
переименуйте файл и запустите скрипт снова; смена `--domain` тоже перерисовывает его
(со снимком старого рядом, `gotcha.bak-<метка>`), и сертификат после неё выпускается заново.

### 10. SELinux и firewalld (только AlmaLinux/Rocky/RHEL)

Этого шага нет на Debian/Ubuntu — SELinux и firewalld там штатно не участвуют в
установке. На EL он идёт сразу за шагом 9, если nginx ставится (без `--no-proxy`),
и до сертификата на шаге 11.

**SELinux.** Если политика в режиме `Enforcing` (проверить: `getenforce`), разрешите
nginx обращаться к приложению на `127.0.0.1:8080` — по умолчанию домен `httpd_t`,
под которым работает nginx, этого не может:

```bash
[ "$(getenforce 2>/dev/null)" = Enforcing ] && setsebool -P httpd_can_network_connect 1
```

**firewalld.** Если он запущен (`firewall-cmd --state` печатает `running`), откройте
порты 80 и 443:

```bash
firewall-cmd --permanent --add-service=http --add-service=https
firewall-cmd --reload
```

Если он не установлен или не запущен — типичный случай для образа AlmaLinux/Rocky
GenericCloud, где firewalld нет из коробки, — инсталлятор печатает предупреждение
и оставляет хост как есть; порты придётся открыть самостоятельно, если их
фильтрует что-то ещё на хосте. Флаг `--no-firewall` пропускает этот шаг молча —
это явная просьба оператора, а не неожиданность.

Оба изменения — глобальные настройки хоста, а не файлы самой установки: `--uninstall`
их не отменяет, ни сам по себе, ни по флагу `--purge`. На них может опираться другой
сервис того же хоста, поэтому откатывать их стоит осознанно, а не одним махом с
удалением gotcha:

```bash
setsebool -P httpd_can_network_connect 0
firewall-cmd --permanent --remove-service=http --remove-service=https
firewall-cmd --reload
```

**EPEL для certbot.** Пакет `python3-certbot-nginx`, нужный на шаге 11, живёт в
EPEL, а не в штатных репозиториях:

```bash
dnf -y repolist enabled 2>/dev/null | grep -qi '^epel' \
  || dnf install -y "https://dl.fedoraproject.org/pub/epel/epel-release-latest-$EL_MAJOR.noarch.rpm"
```

На AlmaLinux и Rocky этого достаточно. На RHEL с активной подпиской часть
зависимостей EPEL требует ещё и включённого репозитория CodeReady Builder — без
него установка `certbot`/`python3-certbot-nginx` может упасть на разрешении
зависимостей:

```bash
subscription-manager repos --enable "codeready-builder-for-rhel-$EL_MAJOR-$(arch)-rpms"
```

Этот путь не прогоняется в CI (нужна активная подписка RHEL, а не только образ) —
держите его в уме как рабочий, а не как проверенный автоматически.

### 11. Получите TLS-сертификат

Это базовая сборка — nginx на 80 плюс сертификат от Let's Encrypt. Тонкая настройка TLS (протоколы, шифры), HSTS и rate-limit на прокси — за пределами этого шага, их настраивает оператор под свои требования.

**На Debian/Ubuntu:**

```bash
DEBIAN_FRONTEND=noninteractive apt-get install -y certbot python3-certbot-nginx
certbot --nginx -d gotcha.example.com -m you@example.com --agree-tos --non-interactive --redirect
```

**На AlmaLinux/Rocky/RHEL 9 и 10:** EPEL из шага 10 уже подключён, дальше так же:

```bash
dnf install -y certbot python3-certbot-nginx
certbot --nginx -d gotcha.example.com -m you@example.com --agree-tos --non-interactive --redirect
```

Отказ certbot не ломает уже поднятый HTTP-стенд на 80 ни на одном семействе —
сертификат можно допровести позже той же командой.

## Установка скриптом

`install-bare-metal.sh` делает ровно шаги 1–11 выше сам, включая идемпотентный повтор (безопасно запускать ещё раз — существующие пароли и ключ не перевыпускаются) и распознавание обновления (если на хосте уже стоит более старая версия — см. [Обновление](/docs/upgrade)).

Скрипт прикладывается к каждому релизу отдельным файлом:

```bash
URL="https://github.com/OtezVikentiy/gotcha/releases/download/vX.Y.Z"
curl -fsSL -o install-bare-metal.sh "$URL/install-bare-metal.sh"
chmod +x install-bare-metal.sh
sudo ./install-bare-metal.sh --version X.Y.Z --domain gotcha.example.com --email you@example.com
```

Без `--domain`/`--email` установится HTTP-стенд без TLS — сертификат можно добавить отдельно позже той же командой `certbot --nginx`.

| Флаг | Значение |
|---|---|
| `--version X.Y.Z` | какой релиз ставить (обязателен, если не указан `--from-tarball`) |
| `--from-tarball PATH` | взять локальный тарбол вместо скачивания |
| `--download-base URL` | другая база загрузки вместо GitHub (зеркало, закрытый контур) |
| `--base-url URL` | явный `GOTCHA_BASE_URL`; без него и без `--domain` скрипт спросит интерактивно (или предупредит и подставит IP хоста с `--yes`) |
| `--domain D` | поставить nginx перед этим доменом, `GOTCHA_BASE_URL` — `https://D` |
| `--email E` | контакт для certbot (требует `--domain`) |
| `--no-proxy` | не ставить и не трогать nginx вовсе |
| `--no-firewall` | не трогать firewalld (только семейство EL) |
| `--skip-databases` | не ставить PostgreSQL/ClickHouse, использовать `--pg-dsn`/`--ch-dsn` — режим «диагностируем, не гарантируем» |
| `--pg-dsn DSN` / `--ch-dsn DSN` | внешние DSN, обязательны вместе с `--skip-databases` |
| `--mem-limit N` | `MemoryMax`/`GOMEMLIMIT` в МиБ (по умолчанию 1024, как `mem_limit: 1g` в Docker-поставке) |
| `--dry-run` | напечатать все команды и содержимое файлов, ничего не менять |
| `--yes` | не спрашивать интерактивно (для CI и автоматизации) |
| `--no-backup` | пропустить `pg_dump` перед обновлением |
| `--force-version` | разрешить установку версии старше самого скрипта |
| `--uninstall` | снять установку (данные и базы остаются) |
| `--purge` | вместе с `--uninstall` — снести и данные, и базы |

## Самопроверка

После установки (скриптом или руками) убедитесь, что всё поднялось:

```bash
/usr/local/bin/gotcha --healthcheck
curl -sf http://127.0.0.1:8080/readyz
```

Ответ `/readyz` вида `{"clickhouse":"ok","postgres":"ok","status":"ready","version":"X.Y.Z"}` означает, что приложение видит обе базы. Если ставили nginx — то же самое, но через домен: `curl -sf https://gotcha.example.com/readyz`.

Поле `version` в ответе — точная версия сборки, и `/healthz` с `/readyz` отдают её без аутентификации. Сайт nginx, который ставит скрипт, оставляет эти две ручки открытыми наружу намеренно: ими пользуются внешние проверки доступности самого инстанса. `/metrics` и `/version` закрыты, снаружи оба отвечают 403. Если раскрывать версию наружу не хотите — закройте и пробы, см. [Усиление установки](/docs/hardening).

Проверьте, что раздача бинаря агента работает (без этого подключение хостов из UI не заработает, см. [Хосты](/docs/hosts)):

```bash
curl -fsSI http://127.0.0.1:8080/agent/gotcha-agent-linux-amd64
```

Ожидается `200 OK`. Зайдите в UI, создайте организацию и проект, отправьте тестовое событие через DSN проекта — это заодно проверяет права `0700` на `StateDirectory` (`/var/lib/gotcha`): если бы права были шире или уже, чем нужно, приложение либо не смогло бы туда писать, либо это заметил бы аудит. Самый простой практический тест той же директории — сделать выгрузку ошибок проекта (раздел «Выгрузки», см. [Выгрузки](/docs/exports)): она пишет файл в `/var/lib/gotcha/exports` и требует именно этих прав на запись.

## Типичные ошибки

**Регистрация или любая форма отвечает `403`.** Это защита от подделки происхождения запроса: `Origin`/`Referer` должен совпадать с `GOTCHA_BASE_URL`. Если в `/etc/gotcha/gotcha.env` указан не тот адрес, по которому вы на самом деле открываете интерфейс (например, забыли схему, домен без `www` вместо с `www`, или зашли по IP, когда `GOTCHA_BASE_URL` — домен), первый же POST — включая самую первую регистрацию — отклоняется `403`. Поправьте `GOTCHA_BASE_URL` в файле окружения и перезапустите: `systemctl restart gotcha`.

**Первый пользователь.** На чистом инстансе первый, кто зарегистрируется, получает права инстанс-администратора автоматически — независимо от режима самостоятельной регистрации. Все следующие регистрации уже подчиняются `GOTCHA_REGISTRATION_MODE` (см. [Конфигурацию](/docs/configuration)).

**`clickhouse-client` отвечает `Code: 516 … default: Authentication failed`.** У пользователя `default` в ClickHouse задан пароль: либо на вопрос постинстала пакета (он появляется, если ставить пакеты без `DEBIAN_FRONTEND=noninteractive`), либо ClickHouse стоял на этом хосте раньше. Базу можно создать и с паролем — `clickhouse-client --password --query "CREATE DATABASE IF NOT EXISTS gotcha"`. Если пароль неизвестен и пользователь `default` вам не нужен, снимите пароль: `rm -f /etc/clickhouse-server/users.d/default-password.xml && systemctl restart clickhouse-server`. На приложении это не сказывается никак — в ClickHouse оно ходит пользователем `gotcha`.

**Приложение не стартует: `clickhouse ping: code: 516 … gotcha: Authentication failed`.** Пароль в `GOTCHA_CH_DSN` не совпадает с паролем пользователя `gotcha`. После установки скриптом действующий пароль лежит в самом `/etc/gotcha/gotcha.env` — правьте DSN по нему. После ручной установки восстановить пароль неоткуда (в `users.d/10-gotcha.xml` только SHA-256) — выпустите новый:

```bash
CH_PASSWORD=$(openssl rand -hex 24)
HASH=$(printf '%s' "$CH_PASSWORD" | sha256sum | awk '{print $1}')
TAG=password_sha256_hex
DSN="clickhouse://gotcha:$CH_PASSWORD@127.0.0.1:9000/gotcha"
sed -i "s#<$TAG>[a-f0-9]*</$TAG>#<$TAG>$HASH</$TAG>#" \
  /etc/clickhouse-server/users.d/10-gotcha.xml
systemctl restart clickhouse-server
sed -i "s#^GOTCHA_CH_DSN=.*#GOTCHA_CH_DSN=$DSN#" /etc/gotcha/gotcha.env
systemctl restart gotcha
```

**Установка обрывается с кодом 5 и текстом `port 5432 listens on 0.0.0.0:5432, not loopback only`** (то же про 8123 и 9000). После установки баз скрипт проверяет, на каких адресах они на самом деле слушают, и отказывается идти дальше, если это не `127.0.0.1`/`::1`. Отказ означает ровно одно: на хосте уже был PostgreSQL или ClickHouse, настроенный на все интерфейсы, и скрипт его переиспользовал — а значит обещание «базы наружу не торчат» для этой установки неверно. Проверка нужна именно потому, что молча получить базу на публичном адресе хуже, чем оборванную установку.

Что делать — одно из двух:

- вернуть базу на loopback и запустить скрипт заново (он идемпотентен): у PostgreSQL это `listen_addresses = 'localhost'` в `postgresql.conf` (или в своём файле в `conf.d/`) и `systemctl restart postgresql`, у ClickHouse — `<listen_host>` в `/etc/clickhouse-server/config.d/` и `systemctl restart clickhouse-server`;
- если внешний доступ к этой базе нужен осознанно и она не «наша», поставить приложение с `--skip-databases` и своими `--pg-dsn`/`--ch-dsn` — тогда скрипт базы не трогает и не проверяет, а ответственность за их доступность и настройку остаётся на операторе.

## Диагностика

Три источника логов:

```bash
journalctl -u gotcha --no-pager -n 100
journalctl -u postgresql --no-pager -n 50
journalctl -u clickhouse-server --no-pager -n 50
```

Ход самой установки (переживает даже обрыв процесса) — в `/var/log/gotcha-install.log`: список выполненных шагов с таймстампами, полезен, если установка прервалась на середине.

Если инсталлятор упал — он печатает список уже сделанных шагов и код завершения. Повторный запуск с теми же флагами идемпотентен: уже сделанное не переделывается, отказавший шаг подхватится с того же места.

## Снятие установки

```bash
sudo ./install-bare-metal.sh --uninstall
```

Снимает юнит и бинарь `gotcha` и выключает сайт nginx (`/etc/nginx/sites-enabled/gotcha`), перезагружая конфиг: иначе хост отвечал бы 502 на всё, ведь дефолтный сайт nginx скрипт при установке снял. Сам файл `/etc/nginx/sites-available/gotcha` остаётся — в нём лежит TLS-блок certbot, который пригодится при возврате. PostgreSQL и ClickHouse и данные в них не трогаются.

Чтобы снести и их: `--uninstall --purge` — необратимо удаляет роль и базу `gotcha` в PostgreSQL, базу `gotcha` в ClickHouse, системного пользователя `gotcha`, каталоги `/var/lib/gotcha`, `/opt/gotcha`, `/etc/gotcha`, журнал установки `/var/log/gotcha-install.log`, а также конфиги, которые скрипт положил в каталоги чужих пакетов: `conf.d/10-gotcha.conf` у PostgreSQL, `config.d/00-common.xml` и `config.d/10-small.xml` у ClickHouse, systemd-override `clickhouse-server.service.d/override.conf`. СУБД при этом не перезапускаются — выбор момента за оператором, и до перезапуска они продолжают работать со старыми настройками.

Что остаётся намеренно: пакеты СУБД и nginx (на хосте ими может пользоваться что-то ещё), apt-репозитории PGDG и ClickHouse вместе со своими keyring-файлами (снять только keyring значило бы сломать `apt-get update`), файл сайта в `sites-available` и каталоги данных самих СУБД.

## Что дальше

- [Конфигурация](/docs/configuration) — полный список переменных окружения (те же имена, что в файле `/etc/gotcha/gotcha.env` выше).
- [Резервное копирование и восстановление](/docs/backup-restore).
- [Обновление](/docs/upgrade).
- [Усиление установки](/docs/hardening).
- [Хосты](/docs/hosts) — подключение серверов через агент.
- Развернуть через Docker вместо этого — [Установка](/docs/installation).
