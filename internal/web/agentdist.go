package web

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// installShScript — POSIX sh скрипт установки/обновления gotcha-agent.
// Встраивается статично: endpoint/ключ инстанса в него НЕ зашиты — их несёт
// env-часть команды установки, которую показывает UI (спека §3.1), сам
// install.sh одинаков для всех инстансов и версий продукта.
//
//go:embed install.sh
var installShScript []byte

// agentDistAllowlist — единственные имена, отдаваемые GET /agent/{file}.
// Строгая мапа, а не проверка "файл существует в AgentDistDir": каталог
// может (по ошибке оператора или сборки) содержать что угодно ещё, а роут не
// обязан становиться file-серверов произвольного содержимого.
var agentDistAllowlist = map[string]bool{
	"gotcha-agent-linux-amd64": true,
	"gotcha-agent-linux-arm64": true,
	"SHA256SUMS":               true,
}

// agentDistHint — тело 404, когда AgentDistDir не сконфигурирован/не
// существует: отличает «эта установка не собрана в образ с бинарями агента»
// (dev-режим `go run` без docker) от «опечатка в имени файла».
const agentDistHint = "agent binaries are not bundled in this build"

// agentETagEntry — результат sha256 одного файла, посчитанный лениво один раз
// (см. Handler.agentETags).
type agentETagEntry struct {
	once sync.Once
	etag string
	err  error
}

// installSh отдаёт install.sh — устанавливает gotcha-agent на хост
// (curl .../install.sh | sh, с переменными окружения GOTCHA_AGENT_* перед
// командой — их даёт UI). Сам embed-скрипт статичен для всех инстансов,
// поэтому единственная переменная в ответе — доступность самой раздачи:
// реальные бинари в комплекте только когда собран Docker-образ (AgentDistDir).
func (h *Handler) installSh(w http.ResponseWriter, r *http.Request) {
	if !h.agentDistAvailable() {
		http.Error(w, agentDistHint, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/x-sh")
	_, _ = w.Write(installShScript)
}

// agentFile отдаёт один бинарь агента (или SHA256SUMS) из AgentDistDir.
// {file} — единственный сегмент пути (шаблон ServeMux "GET /agent/{file}", не
// "{file...}"), поэтому попытки обхода вида "../.." в него не попадают: они
// либо не матчат сам паттерн, либо чистятся редиректом net/http раньше, чем
// доходят сюда (см. agentdist_test.go). Имя дополнительно проверяется по
// agentDistAllowlist — на файловую систему идём только с доверенным именем.
func (h *Handler) agentFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !agentDistAllowlist[name] {
		h.notFound(w, r)
		return
	}
	if !h.agentDistAvailable() {
		http.Error(w, agentDistHint, http.StatusNotFound)
		return
	}

	path := filepath.Join(h.AgentDistDir, name)
	f, err := os.Open(path)
	if err != nil {
		// ENOENT здесь и накрывает «AgentDistDir указан, но каталог не
		// существует» (проверка выше — только на пустую строку), и «сам
		// файл в каталоге отсутствует» — оба случая одинаково 404-с-
		// подсказкой: разбираться оператору, а не атакующему.
		http.Error(w, agentDistHint, http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		h.notFound(w, r)
		return
	}

	etag, err := h.agentFileETag(name, path)
	if err != nil {
		h.notFound(w, r)
		return
	}

	w.Header().Set("ETag", etag)
	// no-cache (не no-store) обязан перекрыть securityHeaders: тот ставит
	// Cache-Control: no-store на ВСЕ ответы шелла (см. web.go), а 10 МиБ
	// бинаря заново на каждое обновление агента — не то, чего мы хотим.
	// no-cache = браузер/curl вправе закешировать тело, но обязан
	// ревалидировать через ETag (см. If-None-Match ниже) — конкретно то, что
	// нужно для неизменяемых, но версионируемых по имени файлов.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "application/octet-stream")

	// Глобальный WriteTimeout сервера — 60с (cmd/gotcha/server.go), а бинарь
	// агента ~9.3 МиБ на узком канале клиента может не уложиться. Продлеваем
	// дедлайн точечно для этого конкретного ответа — но умеренно: 3 минуты
	// дают ~53 КиБ/с (9.3 МиБ / 180с), с запасом для любого живого канала;
	// раздача теперь и так под собственным per-IP лимитером
	// (GOTCHA_DIST_RATE_PER_MIN, дефолт 120/мин, настраивается
	// отдельно от общего publicLimiter — см. Handler.agentLimiter), но
	// длинный дедлайн сам по себе умножает
	// эффект медленного клиента на held-open соединение — держим его коротким.
	//
	// Ошибку игнорируем осознанно: httptest.ResponseRecorder (юнит-тесты
	// остальных обработчиков пакета, если когда-нибудь дернут этот хендлер
	// напрямую) не реализует http.ResponseController-контракт и вернёт
	// http.ErrNotSupported — раздача обязана работать и без продлённого
	// дедлайна (тогда действует дефолтный WriteTimeout), а не падать.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))

	// http.ServeContent сам обрабатывает If-None-Match против уже
	// выставленного заголовка ETag (302→304) и Range (докачка curl -C -).
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// agentDistAvailable — сконфигурирован ли каталог раздачи и существует ли он
// физически. Общая проверка installSh и agentFile.
func (h *Handler) agentDistAvailable() bool {
	if h.AgentDistDir == "" {
		return false
	}
	info, err := os.Stat(h.AgentDistDir)
	return err == nil && info.IsDir()
}

// agentFileETag — sha256 файла по пути path, посчитанный лениво один раз на
// имя name (файлы в раздаваемом образе неизменяемы — пересчитывать хэш на
// каждый запрос незачем). sync.Map, а не обычная мапа с мьютексом: конкурентные
// первые запросы на разные имена не должны блокировать друг друга дольше, чем
// нужно на сам sync.Once конкретного имени.
func (h *Handler) agentFileETag(name, path string) (string, error) {
	v, _ := h.agentETags.LoadOrStore(name, &agentETagEntry{})
	entry := v.(*agentETagEntry)
	entry.once.Do(func() {
		f, err := os.Open(path)
		if err != nil {
			entry.err = err
			return
		}
		defer f.Close()
		hasher := sha256.New()
		if _, err := io.Copy(hasher, f); err != nil {
			entry.err = err
			return
		}
		entry.etag = `"` + hex.EncodeToString(hasher.Sum(nil)) + `"`
	})
	return entry.etag, entry.err
}
