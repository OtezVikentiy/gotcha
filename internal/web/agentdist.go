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

// endpoint/ключ инстанса не зашиты — их несёт env-часть команды из UI;
// install.sh одинаков для всех инстансов и версий.
//
//go:embed install.sh
var installShScript []byte

// строгая мапа, не проверка «файл существует в AgentDistDir» — каталог может
// содержать что угодно ещё, роут не обязан отдавать произвольное содержимое.
var agentDistAllowlist = map[string]bool{
	"gotcha-agent-linux-amd64": true,
	"gotcha-agent-linux-arm64": true,
	"SHA256SUMS":               true,
}

// отличает «агент не собран в этот образ» (dev `go run`) от «опечатка в имени».
const agentDistHint = "agent binaries are not bundled in this build"

type agentETagEntry struct {
	once sync.Once
	etag string
	err  error
}

// install.sh статичен для всех инстансов — единственная переменная тут:
// доступна ли раздача вообще (AgentDistDir, собранный Docker-образ).
func (h *Handler) installSh(w http.ResponseWriter, r *http.Request) {
	if !h.agentDistAvailable() {
		http.Error(w, agentDistHint, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/x-sh")
	_, _ = w.Write(installShScript)
}

// {file} — один сегмент пути ("GET /agent/{file}", не "{file...}"), обход
// вида "../.." сюда не попадает; имя всё равно сверяется с agentDistAllowlist.
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
		// ENOENT покрывает и «каталог не существует», и «файла в нём нет» — оба
		// одинаковый 404 с подсказкой, разбираться оператору, не атакующему.
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
	// no-cache должен перекрыть securityHeaders (no-store на все ответы) — иначе
	// клиент качал бы бинарь заново на каждое обновление вместо ревалидации по ETag.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "application/octet-stream")

	// глобальный WriteTimeout 60с мал для крупного бинаря — продлеваем до 3 мин;
	// ошибку игнорируем: ResponseRecorder не поддерживает SetWriteDeadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))

	// http.ServeContent сам обрабатывает If-None-Match против уже
	// выставленного заголовка ETag (302→304) и Range (докачка curl -C -).
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (h *Handler) agentDistAvailable() bool {
	if h.AgentDistDir == "" {
		return false
	}
	info, err := os.Stat(h.AgentDistDir)
	return err == nil && info.IsDir()
}

// файлы неизменяемы — хэш считается лениво один раз на имя. sync.Map, не
// мьютекс+мапа: конкурентные запросы разных имён не блокируют друг друга.
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
