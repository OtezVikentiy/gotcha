package ingest

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
)

// Набор закрыт — контракт label path метрики gotcha_ingest_deprecated_path_total;
// значение — паттерн регистрации, не сырой URL (иначе взрыв кардинальности).
type DeprecatedPath string

const (
	DeprecatedLogs         DeprecatedPath = "/logs"
	DeprecatedProfilePprof DeprecatedPath = "/profiles/pprof"
	DeprecatedDeployments  DeprecatedPath = "/api/{project}/deployments/"
)

// RFC 9745 structured-field Date; литерал, не время запроса — иначе клиент не
// сможет сравнить значение с тем, что видел вчера.
const deprecationDate = "@1788134400"

// docs уходит в заголовок Link ответа, canonical — в единственную запись лога.
type deprecatedTarget struct {
	docs      string
	canonical string
}

var deprecatedTargets = map[DeprecatedPath]deprecatedTarget{
	DeprecatedLogs:         {docs: "/docs/logs", canonical: "/api/v1/logs"},
	DeprecatedProfilePprof: {docs: "/docs/profiling", canonical: "/api/v1/profiles/pprof"},
	DeprecatedDeployments:  {docs: "/docs/deployments", canonical: "/api/v1/{project}/deployments"},
}

// Self-метрики процесс-локальны и без метки проекта — оператору проекта нужно
// видеть, что именно его отправитель ещё не переехал на канон.
var deprecatedKinds = map[DeprecatedPath]ingestsignal.Kind{
	DeprecatedLogs:         ingestsignal.KindDeprecatedLogs,
	DeprecatedProfilePprof: ingestsignal.KindDeprecatedPprof,
	DeprecatedDeployments:  ingestsignal.KindDeprecatedDeployments,
}

func kindForDeprecated(p DeprecatedPath) (ingestsignal.Kind, bool) {
	k, ok := deprecatedKinds[p]
	return k, ok
}

// Единственный источник соответствия — deprecatedKinds выше; обратный индекс
// строится из него, чтобы вызывающие не держали свою ручную копию.
var deprecatedPathsByKind = reverseDeprecatedKinds()

func reverseDeprecatedKinds() map[ingestsignal.Kind]DeprecatedPath {
	m := make(map[ingestsignal.Kind]DeprecatedPath, len(deprecatedKinds))
	for p, k := range deprecatedKinds {
		m[k] = p
	}
	return m
}

// ok=false — kind вне закрытого набора deprecatedKinds (не про устаревший путь).
func PathForDeprecatedKind(k ingestsignal.Kind) (DeprecatedPath, bool) {
	p, ok := deprecatedPathsByKind[k]
	return p, ok
}

type deprecatedCtxKey struct{}

// Путь известен раньше projectID (аутентификация ещё не пройдена) — сигнал
// пишется на projectID, который резолвится только внутри authenticate.
func withDeprecatedPath(ctx context.Context, p DeprecatedPath) context.Context {
	return context.WithValue(ctx, deprecatedCtxKey{}, p)
}

// ok=false — запрос пришёл на канонический путь, алиас его не оборачивал.
func deprecatedPathFromContext(ctx context.Context) (DeprecatedPath, bool) {
	p, ok := ctx.Value(deprecatedCtxKey{}).(DeprecatedPath)
	return p, ok
}

func (h *Handler) touchDeprecatedSignal(ctx context.Context, projectID int64) {
	p, ok := deprecatedPathFromContext(ctx)
	if !ok {
		return
	}
	kind, ok := kindForDeprecated(p)
	if !ok {
		return
	}
	h.touchSignal(projectID, kind)
}

// Слайс, не обход карты: у обхода карты порядок случайный, и набор меток в
// /metrics плавал бы от перезапуска к перезапуску.
var deprecatedPaths = []DeprecatedPath{
	DeprecatedLogs,
	DeprecatedProfilePprof,
	DeprecatedDeployments,
}

// Копия — чтобы вызывающий не мог мутировать общий слайс.
func DeprecatedPaths() []DeprecatedPath {
	return append([]DeprecatedPath(nil), deprecatedPaths...)
}

// ok=false для пути вне закрытого набора deprecatedTargets.
func DocsPath(p DeprecatedPath) (string, bool) {
	t, ok := deprecatedTargets[p]
	return t.docs, ok
}

func newDeprecatedCounters() map[DeprecatedPath]*atomic.Int64 {
	m := make(map[DeprecatedPath]*atomic.Int64, len(deprecatedPaths))
	for _, p := range deprecatedPaths {
		m[p] = new(atomic.Int64)
	}
	return m
}

func newDeprecatedLogOnce() map[DeprecatedPath]*sync.Once {
	m := make(map[DeprecatedPath]*sync.Once, len(deprecatedPaths))
	for _, p := range deprecatedPaths {
		m[p] = new(sync.Once)
	}
	return m
}

func (h *Handler) DeprecatedPathHits(p DeprecatedPath) int64 {
	if c, ok := h.deprecated[p]; ok {
		return c.Load()
	}
	return 0
}

// Обёртка ничего не решает о запросе — аутентификация, лимитер, квота и коды
// ответов остаются как в каноне; CORS здесь по-прежнему не появляется.
func (h *Handler) deprecatedAlias(p DeprecatedPath, next http.HandlerFunc) http.HandlerFunc {
	target := deprecatedTargets[p]
	return func(w http.ResponseWriter, r *http.Request) {
		// Заголовки — до next: после записи статуса/тела правки в них теряются.
		head := w.Header()
		head.Set("Deprecation", deprecationDate)
		head.Set("Link", "<"+target.docs+">; rel=\"deprecation\"")
		if c, ok := h.deprecated[p]; ok {
			c.Add(1)
		}
		// Раз на путь за жизнь процесса: пер-запросный лог был бы усилителем
		// нагрузки, старые пути принимают телеметрию с той же частотой, что и новые.
		if once, ok := h.deprecatedLogged[p]; ok {
			once.Do(func() {
				slog.Warn("ingest: deprecated path used, switch the sender before 2.0",
					"path", string(p), "canonical", target.canonical)
			})
		}
		// До next: next читает путь из контекста при успехе аутентификации, а
		// до неё projectID ещё не известен.
		next(w, r.WithContext(withDeprecatedPath(r.Context(), p)))
	}
}
