package log

import "time"

// LogRecord — одна строка лога после нормализации из источника (OTLP,
// self-агент, будущие парсеры). Без ProjectID: как и MetricPoint, запись сама
// по себе анонимна, проект передаётся отдельно в Writer.Add(projectID, r) —
// так же, как metric.Writer.Add(projectID, MetricPoint).
type LogRecord struct {
	Timestamp  time.Time // время события по данным источника
	ObservedTS time.Time // время получения записи коллектором/агентом

	Severity       string // канон (Sev*), см. severity.go
	SeverityNumber uint8  // сырой OTLP SeverityNumber, для отладки/аудита
	SeverityText   string // сырой текст severity от источника, до канонизации

	Body string // текст сообщения лога

	TraceID string // склейка с трейсами, если источник её передал
	SpanID  string

	Service     string
	Environment string

	LogAttributes map[string]string // атрибуты самой записи лога
	ResourceAttrs map[string]string // атрибуты ресурса (host, service.*, ...)
}
