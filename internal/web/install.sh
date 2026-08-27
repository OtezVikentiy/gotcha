#!/bin/sh
# gotcha-agent installer. Идемпотентен: повторный запуск без переменных
# окружения — путь обновления (бинарь свежий, конфиг нетронут).
set -eu

CONF=/etc/gotcha-agent/gotcha-agent.env
BIN=/usr/local/bin/gotcha-agent
UNIT=/etc/systemd/system/gotcha-agent.service

fail() { echo "gotcha-agent install: $*" >&2; exit 1; }

# reject_newline — блокирует значения GOTCHA_AGENT_* с embedded LF: конфиг
# пишется построчно printf'ом, лишний перевод строки в значении означает
# инъекцию произвольной ДОПОЛНИТЕЛЬНОЙ строки в $CONF (ревью T11 #1).
reject_newline() {
    if [ "$(printf '%s' "$1" | wc -l)" -gt 0 ]; then
        fail "GOTCHA_AGENT_* values must not contain a newline (would inject extra lines into $CONF)"
    fi
    # CR — тот же вектор, что LF: systemd EnvironmentFile построчный, и
    # одинокий \r внутри значения переживает запись printf'ом целым и невредимым
    # (wc -l его не считает), но на некоторых парсерах ведёт себя как разрыв строки.
    case "$1" in
        *"$(printf '\r')"*)
            fail "GOTCHA_AGENT_* values must not contain a carriage return (would inject extra lines into $CONF)"
            ;;
    esac
}

main() {
    [ "$(uname -s)" = Linux ] || fail "only Linux is supported"
    command -v systemctl >/dev/null 2>&1 || fail "systemd is required"
    command -v curl >/dev/null 2>&1 || fail "curl is required"
    # Тоже нужны, но позже — после того как скрипт уже начал менять систему
    # (скачал бинарь, создал пользователя). Проверяем здесь же, наверху,
    # вместе с systemd/curl: иначе на системе без shadow-utils/coreutils
    # (минимальные контейнерные образы) установка обрывается на середине,
    # оставляя хост в промежуточном состоянии — ровно то, чего этот
    # preflight-блок и задуман избежать.
    command -v useradd >/dev/null 2>&1 || fail "useradd is required (shadow-utils)"
    command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required (coreutils)"
    command -v install >/dev/null 2>&1 || fail "install is required (coreutils)"
    command -v mktemp >/dev/null 2>&1 || fail "mktemp is required (coreutils)"
    case "$(uname -m)" in
        x86_64) arch=amd64 ;;
        aarch64) arch=arm64 ;;
        *) fail "unsupported architecture $(uname -m) (amd64/arm64 for now)" ;;
    esac

    # root-only системы (минимальный Debian, контейнеры) могут не иметь sudo
    # вовсе — тогда просто выполняем команды напрямую (ревью T11 #2). $SUDO
    # намеренно не в кавычках при вызовах ниже: пустая строка обязана исчезнуть
    # словоразбиением, а не остаться пустым аргументом команды.
    if [ "$(id -u)" = 0 ]; then
        SUDO=""
    else
        command -v sudo >/dev/null 2>&1 || fail "run as root or install sudo"
        SUDO=sudo
    fi

    endpoint="${GOTCHA_AGENT_ENDPOINT:-}"
    key="${GOTCHA_AGENT_KEY:-}"
    if [ -n "$endpoint" ] && [ -n "$key" ]; then
        mode=install
        reject_newline "$endpoint"
        reject_newline "$key"
        reject_newline "${GOTCHA_AGENT_INTERVAL:-}"
        reject_newline "${GOTCHA_AGENT_HOSTNAME:-}"
        reject_newline "${GOTCHA_AGENT_CA_CERT:-}"
        reject_newline "${GOTCHA_AGENT_TLS_SKIP_VERIFY:-}"
        reject_newline "${GOTCHA_AGENT_ENVIRONMENT:-}"
        reject_newline "${GOTCHA_AGENT_ROLE:-}"
    elif [ -z "$endpoint" ] && [ -z "$key" ]; then
        # Опциональные без обязательных — ошибка, не молчаливый игнор (§2.2):
        # иначе GOTCHA_AGENT_INTERVAL=… при обновлении тихо потерялся бы.
        if [ -n "${GOTCHA_AGENT_INTERVAL:-}${GOTCHA_AGENT_HOSTNAME:-}${GOTCHA_AGENT_CA_CERT:-}${GOTCHA_AGENT_TLS_SKIP_VERIFY:-}${GOTCHA_AGENT_ENVIRONMENT:-}${GOTCHA_AGENT_ROLE:-}" ]; then
            fail "optional GOTCHA_AGENT_* without ENDPOINT+KEY have no effect: edit $CONF and run systemctl restart gotcha-agent"
        fi
        $SUDO test -f "$CONF" || fail "$CONF not found and GOTCHA_AGENT_ENDPOINT/GOTCHA_AGENT_KEY not set"
        mode=update
        # Читаем значение без dot-sourcing: конфиг пишется printf'ом без
        # экранирования (значения со пробелами — легальны для systemd
        # EnvironmentFile), а ". $CONF" в шелле развернул бы их как отдельные
        # слова/переразбирал синтаксис — неверный диагноз при валидном
        # значении (ревью T11 #1, воспроизведено на dash). head -1 — на
        # случай дублей строки в вручную отредактированном файле: берём
        # первую, как это делает большинство простых EnvironmentFile-парсеров.
        # cut, в отличие от парсера systemd EnvironmentFile, не снимает
        # обрамляющие кавычки со значения — оператор, вписавший в конфиг
        # руками GOTCHA_AGENT_ENDPOINT="https://…" (валидно для
        # EnvironmentFile), без sed ниже получил бы кавычки прямо внутри
        # curl-URL. Снимаем ОДНУ пару обрамляющих кавычек (двойных или
        # одинарных), если голова и хвост совпадают.
        endpoint=$($SUDO grep '^GOTCHA_AGENT_ENDPOINT=' "$CONF" | head -n 1 | cut -d= -f2- \
            | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/")
        [ -n "$endpoint" ] || fail "$CONF has no GOTCHA_AGENT_ENDPOINT"
    else
        fail "both GOTCHA_AGENT_ENDPOINT and GOTCHA_AGENT_KEY are required (partial values are rejected; to change a single setting, edit $CONF and run systemctl restart gotcha-agent)"
    fi

    tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
    # "--" отделяет опции curl от URL: endpoint приходит из окружения
    # (install-время) или из уже сохранённого конфига (update-время) — без
    # "--" URL, начинающийся с "-", разобрался бы как опция curl.
    curl -fsSL --retry 3 -o "$tmp/gotcha-agent" -- "$endpoint/agent/gotcha-agent-linux-$arch"
    curl -fsSL --retry 3 -o "$tmp/SHA256SUMS" -- "$endpoint/agent/SHA256SUMS"
    # Сверка ловит битую/обрезанную загрузку и прокси-кеши; от компрометации
    # сервера не защищает (суммы едут тем же каналом) — спека §2.2.
    ( cd "$tmp" && grep " gotcha-agent-linux-$arch\$" SHA256SUMS \
        | sed "s/gotcha-agent-linux-$arch/gotcha-agent/" | sha256sum -c - ) \
        || fail "SHA-256 mismatch — download is corrupted, retry"
    # mv из /tmp (tmpfs) в /usr/local/bin — копия через границу ФС, не rename:
    # неатомарно и упирается в ETXTBSY при работающем сервисе. Поэтому install
    # кладёт root:root 0755 в $BIN.new (та же ФС), а rename поверх — атомарен
    # и не трогает исполняемый inode (ревью плана №6).
    $SUDO install -o root -g root -m 755 "$tmp/gotcha-agent" "$BIN.new"
    $SUDO mv "$BIN.new" "$BIN"

    if ! id gotcha-agent >/dev/null 2>&1; then
        $SUDO useradd --system --no-create-home --shell "$(command -v nologin || echo /usr/sbin/nologin)" gotcha-agent
    fi

    if [ "$mode" = install ]; then
        $SUDO mkdir -p /etc/gotcha-agent
        # Файл создаётся с 0600 ДО записи ключа: tee в свежий файл дал бы
        # umask-права (0644), и ключ было бы мгновение читаемо всеми (ревью №7).
        $SUDO install -m 600 -o root -g root /dev/null "$CONF"
        # printf — builtin: значения не попадают в argv ни одного процесса.
        {
            printf 'GOTCHA_AGENT_ENDPOINT=%s\n' "$endpoint"
            printf 'GOTCHA_AGENT_KEY=%s\n' "$key"
            if [ -n "${GOTCHA_AGENT_INTERVAL:-}" ]; then printf 'GOTCHA_AGENT_INTERVAL=%s\n' "$GOTCHA_AGENT_INTERVAL"; fi
            if [ -n "${GOTCHA_AGENT_HOSTNAME:-}" ]; then printf 'GOTCHA_AGENT_HOSTNAME=%s\n' "$GOTCHA_AGENT_HOSTNAME"; fi
            if [ -n "${GOTCHA_AGENT_CA_CERT:-}" ]; then printf 'GOTCHA_AGENT_CA_CERT=%s\n' "$GOTCHA_AGENT_CA_CERT"; fi
            if [ -n "${GOTCHA_AGENT_TLS_SKIP_VERIFY:-}" ]; then printf 'GOTCHA_AGENT_TLS_SKIP_VERIFY=%s\n' "$GOTCHA_AGENT_TLS_SKIP_VERIFY"; fi
            if [ -n "${GOTCHA_AGENT_ENVIRONMENT:-}" ]; then printf 'GOTCHA_AGENT_ENVIRONMENT=%s\n' "$GOTCHA_AGENT_ENVIRONMENT"; fi
            if [ -n "${GOTCHA_AGENT_ROLE:-}" ]; then printf 'GOTCHA_AGENT_ROLE=%s\n' "$GOTCHA_AGENT_ROLE"; fi
        } | $SUDO tee "$CONF" >/dev/null
    fi

    # --check валидирует конфиг ($CONF, свежий или прежний) тем же кодом,
    # что боевой процесс (agent.LoadConfig), без сети и без цикла сбора —
    # ДО systemctl enable. Без этой проверки Type=simple + restart вернут 0
    # мгновенно после exec, даже если агент на битом URL/ключе упадёт через
    # долю секунды — скрипт соврал бы "installed and running" (ops-H2).
    # systemd-run с EnvironmentFile=$CONF прогоняет тот же парсинг
    # окружения, что и боевой юнит ниже (кавычки/пробелы в значениях).
    $SUDO systemd-run --quiet --wait --pipe --collect \
        -p EnvironmentFile="$CONF" \
        "$BIN" --check \
        || fail "config check failed ($BIN --check exited non-zero) — inspect $CONF"

    # Юнит — артефакт установщика, перезаписывается; свои правки — systemctl edit.
    $SUDO tee "$UNIT" >/dev/null <<'EOF'
[Unit]
Description=Gotcha host metrics agent
After=network-online.target
Wants=network-online.target

[Service]
User=gotcha-agent
EnvironmentFile=/etc/gotcha-agent/gotcha-agent.env
ExecStart=/usr/local/bin/gotcha-agent
Restart=always
RestartSec=30
# Код 2 = ошибка конфига (agent.LoadConfig, тот же путь что --check выше):
# рестарт её не лечит, а Restart=always/RestartSec=5 на битом конфиге —
# краш-луп ~17к рестартов/сутки, забивающий journald (ops-H3). Сетевые и
# прочие рантайм-сбои (код 1) по-прежнему рестартятся как обычно.
RestartPreventExitStatus=2
NoNewPrivileges=yes
ProtectSystem=strict
# read-only (не yes): "yes" прячет /home,/root,/var/tmp под tmpfs, из-за чего
# агент не видит их как отдельные разделы диска и они выпадают из порога
# "Диск" (регресс паритета с прежним коллектором на root, ops-H5/sec-M2).
# PrivateTmp не используется вовсе — агенту /tmp не нужен (ProtectSystem=
# strict и так режет запись всюду, кроме нужных путей), а его включение
# по той же причине маскирует /var/tmp как отдельный раздел.
ProtectHome=read-only
# Проба процессов (gopsutil) не критична по задержке — не соревнуется за
# CPU/память с ingest на том же хосте.
Nice=10
CPUWeight=20
MemoryMax=128M

[Install]
WantedBy=multi-user.target
EOF
    $SUDO systemctl daemon-reload
    $SUDO systemctl enable --now gotcha-agent
    # enable --now не перезапускает уже работающий юнит — на update-пути
    # именно restart подхватывает свежий бинарь, залитый выше.
    $SUDO systemctl restart gotcha-agent
    # Type=simple: restart возвращает 0 сразу после exec, до того как агент
    # успел бы упасть на рантайм-ошибке — даём пару секунд дойти хотя бы до
    # первого такта, прежде чем верить, что процесс жив (ops-H2).
    sleep 2
    if ! $SUDO systemctl is-active --quiet gotcha-agent; then
        $SUDO journalctl -u gotcha-agent --no-pager -n 20 || true
        fail "gotcha-agent is not active after restart (see journalctl output above)"
    fi
    echo "gotcha-agent: installed and running (logs: journalctl -u gotcha-agent)"
}

main "$@"
