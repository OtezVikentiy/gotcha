package ingest

import "sync/atomic"

// IngestRejectReason — почему приём отверг (или частично отклонил) запрос,
// огрублённое до причины, которая одинаково читается НЕЗАВИСИМО от вида
// телеметрии. gotcha_ingest_key_rejections_total{reason} (см.
// KeyRejectReason) уже даёт точную причину отказа по ключу, но не говорит,
// какой вход отбивало (событие? лог? метрика?) — приёмник отвечает по-своему
// на каждый из шести входов (envelope/store, /v1/traces, /v1/metrics,
// /v1/logs, /logs, /profiles/pprof, /api/{project}/deployments/), и раньше
// частота отказа по rate-limit/квоте/размеру тела не была видна нигде вовсе
// (J3): дежурный видел только рост 429/413/400 в логе реверс-прокси, без
// возможности отличить "клиента троттлит" от "организация исчерпала квоту".
//
// Набор ЗАКРЫТ и является контрактом self-метрики
// gotcha_ingest_rejected_total{reason,signal}: после 1.0 расширять его
// дорого (см. internal/guards — каждое имя self-метрики пиннится литералом,
// а свежая метка reason ломает дашборды, построенные на перечислении). Шесть
// значений:
//   - key_unknown — ключ приёма не резолвится ни в один проект: клиент не
//     прислал sentry_key/bearer вовсе, прислал опечатанный/несуществующий
//     ключ, либо ключ резолвится, но в чужой проект относительно пути
//     запроса. Три этих случая уже различены подробнее в
//     KeyRejectReason/gotcha_ingest_key_rejections_total — здесь они
//     сознательно СХЛОПНУТЫ в одну причину: с точки зрения "что это за
//     отказ" все три говорят одно и то же клиенту "этот ключ не работает
//     здесь", а разбор ПОЧЕМУ остаётся за старой, более детальной метрикой.
//   - key_revoked — ЗАРЕЗЕРВИРОВАНО. org.Service.KeyByPublic резолвит ключ
//     запросом с условием "revoked_at IS NULL": отозванный ключ и ключ,
//     которого никогда не существовало, сегодня НЕ различимы на этом слое —
//     оба возвращают org.ErrNotFound, и приёмник не может присвоить эту
//     метку ни одному реальному запросу. Значение остаётся в закрытом наборе
//     (а не добавляется отдельной волной после 1.0), чтобы день, когда
//     резолвер начнёт различать "отозван" от "не найден" (например, отдельным
//     запросом на подтверждённом пути отказа — вне горячего пути, дёшево),
//     не потребовал трогать контракт этой метрики: разметка появится, счётчик
//     — уже существует и заранее упомянут в документации.
//   - rate_limit — per-DSN токен-бакет (Handler.rate) отклонил запрос ДО
//     квоты и ДО разбора тела.
//   - quota — организация исчерпала месячную квоту вида телеметрии: запрос
//     получил 429 (writeQuotaExceeded). ЧАСТИЧНОЕ списание квоты (envelope
//     смешанного состава, где по одному классу квота ещё есть) сюда не
//     попадает — see writeQuotaExceeded: это метрика ОТКАЗАННЫХ запросов, а
//     не отброшенных единиц (для последних есть per-org
//     org_usage.dropped_* и process-local countDrop).
//   - too_large — тело запроса превысило GOTCHA_MAX_EVENT_BYTES (или
//     производный от него лимит: сжатое тело, декомпрессированное pprof).
//   - malformed — тело прочитано, но не разобралось: битый JSON/protobuf,
//     повреждённый gzip/zstd-заголовок, пустой envelope.
type IngestRejectReason string

const (
	RejectKeyUnknown IngestRejectReason = "key_unknown"
	RejectKeyRevoked IngestRejectReason = "key_revoked" // зарезервировано, см. докблок типа
	RejectRateLimit  IngestRejectReason = "rate_limit"
	RejectQuota      IngestRejectReason = "quota"
	RejectTooLarge   IngestRejectReason = "too_large"
	RejectMalformed  IngestRejectReason = "malformed"
)

// IngestSignal — вид телеметрии, к которому относится отказ. Значения
// совпадают со строками kind, которыми Handler.grant уже помечает
// org_usage-квоты (event/transaction/profile/metric/log), плюс deploy —
// у деплоя своей квоты нет, но есть auth/rate-limit/тело, как у остальных
// пяти входов.
type IngestSignal string

const (
	SignalEvent       IngestSignal = "event"
	SignalTransaction IngestSignal = "transaction"
	SignalMetric      IngestSignal = "metric"
	SignalProfile     IngestSignal = "profile"
	SignalLog         IngestSignal = "log"
	SignalDeploy      IngestSignal = "deploy"
)

// IngestRejectionKey — одна пара (reason, signal), которую приёмник умеет
// реально произвести (см. IngestRejectionPairs).
type IngestRejectionKey struct {
	Reason IngestRejectReason
	Signal IngestSignal
}

// ingestRejectionPairs — ПОЛНЫЙ и ЗАКРЫТЫЙ список пар (reason, signal),
// которые код приёмника способен инкрементировать. main регистрирует по
// self-метрике на пару (см. KeyRejectReasons/DropReasons — тот же приём:
// счётчики создаются один раз при инициализации, попадание в map на
// горячем пути не требуется).
//
// key_revoked сюда НЕ входит ни для одного сигнала: причина зарезервирована
// в самом типе (IngestRejectReason), но код её не производит — см. докблок
// RejectKeyRevoked. Регистрировать self-метрику, которую ничто и никогда не
// инкрементирует, значило бы обещать оператору наблюдаемость, которой нет.
//
// deploy отсутствует у reason=quota: деплои не расходуют месячную квоту
// (см. deploymentsIngest) — только auth/rate-limit/размер тела/разбор.
var ingestRejectionPairs = []IngestRejectionKey{
	{RejectKeyUnknown, SignalEvent}, {RejectKeyUnknown, SignalTransaction},
	{RejectKeyUnknown, SignalMetric}, {RejectKeyUnknown, SignalProfile},
	{RejectKeyUnknown, SignalLog}, {RejectKeyUnknown, SignalDeploy},

	{RejectRateLimit, SignalEvent}, {RejectRateLimit, SignalTransaction},
	{RejectRateLimit, SignalMetric}, {RejectRateLimit, SignalProfile},
	{RejectRateLimit, SignalLog}, {RejectRateLimit, SignalDeploy},

	{RejectTooLarge, SignalEvent}, {RejectTooLarge, SignalTransaction},
	{RejectTooLarge, SignalMetric}, {RejectTooLarge, SignalProfile},
	{RejectTooLarge, SignalLog}, {RejectTooLarge, SignalDeploy},

	{RejectMalformed, SignalEvent}, {RejectMalformed, SignalTransaction},
	{RejectMalformed, SignalMetric}, {RejectMalformed, SignalProfile},
	{RejectMalformed, SignalLog}, {RejectMalformed, SignalDeploy},

	{RejectQuota, SignalEvent}, {RejectQuota, SignalTransaction},
	{RejectQuota, SignalMetric}, {RejectQuota, SignalProfile},
	{RejectQuota, SignalLog},
}

// IngestRejectionPairs — копия ingestRejectionPairs для main (та же защита
// от чужой мутации общего слайса, что у KeyRejectReasons/DropReasons).
func IngestRejectionPairs() []IngestRejectionKey {
	return append([]IngestRejectionKey(nil), ingestRejectionPairs...)
}

func newIngestRejectCounters() map[IngestRejectionKey]*atomic.Int64 {
	m := make(map[IngestRejectionKey]*atomic.Int64, len(ingestRejectionPairs))
	for _, k := range ingestRejectionPairs {
		m[k] = new(atomic.Int64)
	}
	return m
}

// countRejected увеличивает self-счётчик отказа приёма по (reason, signal).
// Пара вне ingestRejectionPairs молча игнорируется (как countKeyReject) —
// это защита от опечатки в аргументе на горячем пути, а не повод падать.
func (h *Handler) countRejected(reason IngestRejectReason, signal IngestSignal) {
	if c, ok := h.rejected[IngestRejectionKey{reason, signal}]; ok {
		c.Add(1)
	}
}

// RejectedBy — снимок счётчика gotcha_ingest_rejected_total для конкретной
// пары (reason, signal) с начала процесса.
func (h *Handler) RejectedBy(reason IngestRejectReason, signal IngestSignal) int64 {
	if c, ok := h.rejected[IngestRejectionKey{reason, signal}]; ok {
		return c.Load()
	}
	return 0
}
