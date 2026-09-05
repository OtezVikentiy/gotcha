package guards

import (
	"regexp"
	"strings"
	"testing"
)

// httpErrorLiteralRe находит http.Error(w, "...", ...) — вызов с ЛИТЕРАЛЬНОЙ
// строкой сообщения вместо i18n.T/h.renderError. Не матчит http.Error(w,
// someConst, ...) (идентификатор) и http.Error(w, i18n.T(...), ...)
// (аргумент начинается не с кавычки) — только двойная кавычка сразу вторым
// аргументом.
var httpErrorLiteralRe = regexp.MustCompile(`http\.Error\(\s*\w+\s*,\s*"`)

// machineResponseFiles — файлы internal/web, чьи http.Error(...) отвечают не
// браузеру человека, а программе (install-скрипту, воркеру), которая тело
// ответа не рендерит и локаль не спрашивает — переводить его на i18n
// незачем. Список ведётся ЯВНО: добавление файла сюда — осознанное решение
// ревью задачи, а не эвристика по пути/имени функции, которая незаметно
// размывается со временем (T5, волна 3 аудита 2026-09-05, K7-10 — тогда же
// заведён этот сторож).
var machineResponseFiles = map[string]string{
	"internal/web/agentdist.go": "installSh отдаёт install.sh для curl | sh, agentFile — " +
		"бинарь агента напрямую: оба потребителя не рендерят HTML и не смотрят на " +
		"Accept-Language браузера",
	"internal/web/probeapi.go": "API для внешних uptime-проб (лизинг заданий, приём " +
		"результатов) — потребитель probe-раннер, а не браузер; ответы уже JSON через " +
		"writeProbeError, i18n здесь бессмыслен",
	"internal/web/heartbeat.go": "приём heartbeat-пинга от агента/cron — потребитель " +
		"скрипт, а не браузер; ответы уже JSON через writeHeartbeatJSON, i18n здесь " +
		"бессмыслен",
}

// TestNoLiteralHTTPErrorInWeb — находка K7-10: около 86 мест в internal/web
// отвечали http.Error с английским литералом мимо i18n, пользователь с
// русской локалью получал на месте страницы ошибки английский текст.
// Человеческий ответ обязан идти через h.renderError с ключом i18n (см.
// denyCrossOrigin/renderError в web.go) — ключ заводится в ОБЕИХ локалях
// (internal/i18n/locales/ru.json и en.json).
//
// Упадёт так: новый http.Error(w, "текст", ...) в человеческом файле —
// перевести на h.renderError(w, r, status, i18n.T(r.Context(), "error…")).
// Если ответ на самом деле машинный (не для браузера человека, тело не
// рендерится как страница) — внести файл в machineResponseFiles с
// обоснованием, а не менять этот тест.
func TestNoLiteralHTTPErrorInWeb(t *testing.T) {
	tree := Load(t)
	for _, f := range tree.GoFiles {
		if !strings.HasPrefix(f.Path, "internal/web/") || f.Generated ||
			strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		if _, exempt := machineResponseFiles[f.Path]; exempt {
			continue
		}
		lines := strings.Split(f.Body, "\n")
		for i, line := range lines {
			if httpErrorLiteralRe.MatchString(stripTrailingComment(line)) {
				t.Errorf("%s:%d: http.Error с литеральным текстом мимо i18n — переведите на "+
					"h.renderError(w, r, status, i18n.T(r.Context(), \"error.…\")) с ключом в "+
					"обеих локалях, либо, если ответ машинный (не для браузера человека), "+
					"внесите файл в machineResponseFiles с обоснованием: %s",
					f.Path, i+1, strings.TrimSpace(line))
			}
		}
	}
}
