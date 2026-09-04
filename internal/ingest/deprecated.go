package ingest

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
)

// DeprecatedPath — старый путь приёма, оставленный работать алиасом до 1.0.
// Три собственных входа gotcha исторически жили в чужих неймспейсах: /logs и
// /profiles/pprof — в корне рядом с SSR catch-all, /api/{project}/deployments/
// — в неймспейсе совместимости с Sentry SDK. Каноном стал собственный
// неймспейс /api/v1/* (см. Handler.Register); старые пути остаются рабочими,
// но помеченными.
//
// Набор ЗАКРЫТ и является контрактом метки path у self-метрики
// gotcha_ingest_deprecated_path_total. Значение — паттерн регистрации, а не
// конкретный URL запроса: у деплоя путь содержит id проекта, и разворачивать
// его в метку значило бы взорвать кардинальность метрики.
//
// Метрика и весь этот файл ВРЕМЕННЫЕ: они умирают вместе с алиасами в 1.0.
// Это сказано и в /docs/self-monitoring обеих локалей, и в CHANGELOG — имя
// self-метрики после задачи 1 контрактного прохода является обещанием
// оператору, и обещание «этот счётчик исчезнет» обязано быть дано вслух в тот
// же момент, когда счётчик вводится.
type DeprecatedPath string

const (
	DeprecatedLogs         DeprecatedPath = "/logs"
	DeprecatedProfilePprof DeprecatedPath = "/profiles/pprof"
	DeprecatedDeployments  DeprecatedPath = "/api/{project}/deployments/"
)

// deprecationDate — день, когда пути объявлены устаревшими
// (2026-08-31T00:00:00Z), в форме structured-field Date из RFC 9745.
// ЛИТЕРАЛ, а не время запроса: значение обязано быть одинаковым во всех
// ответах и на всех инстансах, иначе клиент не сможет сравнить его с тем, что
// видел вчера.
const deprecationDate = "@1788134400"

// deprecatedTarget — куда вести оператора, наткнувшегося на устаревший путь:
// docs — страница документации СВОЕГО входа (её адрес уходит в заголовок Link),
// canonical — путь, на который надо перейти (уходит в единственную запись лога).
type deprecatedTarget struct {
	docs      string
	canonical string
}

var deprecatedTargets = map[DeprecatedPath]deprecatedTarget{
	DeprecatedLogs:         {docs: "/docs/logs", canonical: "/api/v1/logs"},
	DeprecatedProfilePprof: {docs: "/docs/profiling", canonical: "/api/v1/profiles/pprof"},
	DeprecatedDeployments:  {docs: "/docs/deployments", canonical: "/api/v1/{project}/deployments"},
}

// deprecatedKinds — соответствие устаревшего пути виду per-project сигнала
// (аудит перед 1.0, K7-5/K7-6): self-метрики gotcha_ingest_deprecated_path_total
// процесс-локальны и без метки проекта, а оператору конкретного проекта нужно
// видеть, что именно ЕГО отправитель ещё не переехал на канон.
var deprecatedKinds = map[DeprecatedPath]ingestsignal.Kind{
	DeprecatedLogs:         ingestsignal.KindDeprecatedLogs,
	DeprecatedProfilePprof: ingestsignal.KindDeprecatedPprof,
	DeprecatedDeployments:  ingestsignal.KindDeprecatedDeployments,
}

// kindForDeprecated отдаёт вид сигнала для устаревшего пути p. ok=false для
// пути вне закрытого набора (тот же контракт, что у deprecatedTargets[p]).
func kindForDeprecated(p DeprecatedPath) (ingestsignal.Kind, bool) {
	k, ok := deprecatedKinds[p]
	return k, ok
}

// deprecatedCtxKey — приватный ключ контекста запроса под DeprecatedPath.
type deprecatedCtxKey struct{}

// withDeprecatedPath кладёт p в контекст запроса: deprecatedAlias знает путь
// раньше, чем next() узнаёт projectID (аутентификация ещё не пройдена), а
// сигнал устаревшего пути пишется на projectID, который выясняется только
// внутри authenticate/otlpAuthenticate — контекст переносит p через границу.
func withDeprecatedPath(ctx context.Context, p DeprecatedPath) context.Context {
	return context.WithValue(ctx, deprecatedCtxKey{}, p)
}

// deprecatedPathFromContext читает DeprecatedPath, положенный
// withDeprecatedPath. ok=false — запрос пришёл на канонический путь (алиас
// его не оборачивал).
func deprecatedPathFromContext(ctx context.Context) (DeprecatedPath, bool) {
	p, ok := ctx.Value(deprecatedCtxKey{}).(DeprecatedPath)
	return p, ok
}

// touchDeprecatedSignal — если запрос пришёл через deprecatedAlias, отмечает
// per-project сигнал устаревшего пути на projectID. Общая точка для
// authenticate и otlpAuthenticate: оба протокола (Sentry-стиль и OTLP-Bearer)
// оборачиваются deprecatedAlias, и оба узнают projectID только по итогу
// собственной успешной аутентификации.
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

// deprecatedPaths — порядок регистрации self-метрик в main. Явный слайс, а не
// обход карты: у обхода карты порядок случайный, и набор меток в /metrics
// плавал бы от перезапуска к перезапуску.
var deprecatedPaths = []DeprecatedPath{
	DeprecatedLogs,
	DeprecatedProfilePprof,
	DeprecatedDeployments,
}

// DeprecatedPaths — копия набора для main (та же защита от чужой мутации
// общего слайса, что у KeyRejectReasons/IngestRejectionPairs).
func DeprecatedPaths() []DeprecatedPath {
	return append([]DeprecatedPath(nil), deprecatedPaths...)
}

// DocsPath — страница документации СВОЕГО входа для устаревшего пути p (тот
// же адрес, что уходит в заголовок Link deprecatedAlias, см. deprecatedTarget
// выше). Единственный потребитель вне пакета — web.deprecatedPathsView
// (аудит перед 1.0, K7-5): callout на странице настроек проекта должен вести
// на страницу конкретного входа, а не на общий /docs/upgrade. ok=false для
// пути вне закрытого набора deprecatedTargets.
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

// DeprecatedPathHits — снимок счётчика gotcha_ingest_deprecated_path_total по
// одному пути с начала процесса.
func (h *Handler) DeprecatedPathHits(p DeprecatedPath) int64 {
	if c, ok := h.deprecated[p]; ok {
		return c.Load()
	}
	return 0
}

// deprecatedAlias оборачивает хендлер приёма, зарегистрированный на СТАРОМ
// пути. Обёртка НИЧЕГО не решает о запросе: аутентификация, per-DSN лимитер и
// квота остаются внутри хендлера и одинаковы для канона и алиаса — они и так
// выбираются не по пути, а по значению IngestSignal. Коды ответов, тела и
// семантика разбора не меняются; CORS здесь не появляется — как и раньше,
// OPTIONS для этих путей не регистрируется ни на старой форме, ни на новой.
func (h *Handler) deprecatedAlias(p DeprecatedPath, next http.HandlerFunc) http.HandlerFunc {
	target := deprecatedTargets[p]
	return func(w http.ResponseWriter, r *http.Request) {
		// Заголовки — ДО next: хендлер пишет статус и тело, после чего карта
		// заголовков уже отправлена и правки в неё теряются.
		head := w.Header()
		head.Set("Deprecation", deprecationDate)
		head.Set("Link", "<"+target.docs+">; rel=\"deprecation\"")
		if c, ok := h.deprecated[p]; ok {
			c.Add(1)
		}
		// Лог — один раз на путь за жизнь процесса. Объём несёт метрика; запись
		// существует, чтобы оператор, читающий журнал после обновления, увидел
		// факт, не идя в /metrics. Пер-запросный лог был бы усилителем нагрузки:
		// старые пути принимают телеметрию с той же частотой, что и новые.
		if once, ok := h.deprecatedLogged[p]; ok {
			once.Do(func() {
				slog.Warn("ingest: deprecated path used, switch the sender before 1.0",
					"path", string(p), "canonical", target.canonical)
			})
		}
		// DeprecatedPath в контекст — ДО next: authenticate/otlpAuthenticate
		// внутри next читают его при успехе, чтобы отметить per-project сигнал
		// (K7-5/K7-6, см. touchDeprecatedSignal). ProjectID на этом шаге ещё не
		// известен — next его ещё не резолвил.
		next(w, r.WithContext(withDeprecatedPath(r.Context(), p)))
	}
}
