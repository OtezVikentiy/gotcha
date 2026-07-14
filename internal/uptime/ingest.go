package uptime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Ingestor — общий хвост обработки результата проверки: ClaimJob → буфер CH →
// ApplyResult → OnResult (детекция). Через него идут ОБА источника
// результатов — локальная проба (Runner.runOne) и выносная (HTTP-эндпойнт
// /probe/results), поэтому детекция инцидентов и запись в ClickHouse
// существуют ровно в одном экземпляре: центр обрабатывает присланный пробой
// результат тем же кодом, что и свой собственный.
//
// Само выполнение проверки (Checker) в Ingestor не входит — он принимает уже
// готовый Result: у выносной пробы чекер отработал на её стороне.
type Ingestor struct {
	Svc    *Service
	Writer *ResultWriter // может быть nil — тогда без записи в CH
	// OnResult — колбэк детекции (uptime.Detector.OnResult в проде), после
	// ApplyResult; nil — ничего не делает.
	OnResult func(ctx context.Context, m Monitor, region string, r Result, st State)
}

// Accept проводит результат r задания j (снятое с очереди временем at,
// которое ставит ЦЕНТР, а не проба) через весь хвост обработки — но СНАЧАЛА
// забирает задание себе (ClaimJob) и только потом трогает что-либо ещё.
// Claim и есть завершение задания: удавшийся claim снимает строку с очереди и
// делает этот вызов единственным, кому позволено применить результат.
//
// Claim не удался (false, без ошибки) — задание уже применено параллельным
// вызовом или перевыдано после истечения lease: результат молча
// выбрасывается, БЕЗ записи в ClickHouse, БЕЗ ApplyResult и БЕЗ OnResult.
// Иначе одна реальная проверка увеличила бы consecutive_fails дважды
// (ApplyResult атомарен, но не идемпотентен) и дважды дёрнула бы детектор —
// см. ClaimJob.
//
// Компромисс: если ApplyResult упадёт уже ПОСЛЕ claim'а, результат этой
// проверки потерян — задание из очереди снято, и монитор будет проверен
// заново, когда планировщик поставит его в очередь по следующему сроку. Это
// сознательно: потерять одну проверку дешевле, чем применить её дважды.
// Держать транзакцию открытой на всё время записи в CH и вызова детектора мы
// не хотим (это внешние по отношению к PG вызовы).
func (i *Ingestor) Accept(ctx context.Context, j Job, at time.Time, r Result) error {
	claimed, err := i.Svc.ClaimJob(ctx, j.QueueID, j.LeaseUntil)
	if err != nil {
		return fmt.Errorf("uptime: ingest: claim job: %w", err)
	}
	if !claimed {
		slog.Info("uptime: ingest: job already claimed or re-leased, result dropped",
			"monitor_id", j.MonitorID, "region", j.Region, "queue_id", j.QueueID)
		return nil
	}

	if i.Writer != nil {
		i.Writer.Add(j.Monitor.ProjectID, j.MonitorID, j.Region, at, r)
	}

	st, err := i.Svc.ApplyResult(ctx, j.MonitorID, j.Region, r.OK, r.Error, at)
	if err != nil {
		return fmt.Errorf("uptime: ingest: apply result: %w", err)
	}

	if i.OnResult != nil {
		i.OnResult(ctx, j.Monitor, j.Region, r, st)
	}
	return nil
}

// Ниже — DTO протокола проб (спека §4). Живут в uptime, а не в web: их
// используют обе стороны — серверные ручки /probe/lease и /probe/results
// (internal/web/probeapi.go) и клиент выносной пробы, — и формат обязан быть
// один на всех.

// LeaseRequest — тело POST /probe/lease. Limit ≤ 0 означает «сколько дашь»
// (сервер подставит свой дефолт и обрежет по своему максимуму).
type LeaseRequest struct {
	Limit int `json:"limit"`
}

// JobDTO — задание, выданное пробе. Всё, что нужно чекеру, и ничего лишнего:
// ни project_id, ни порогов, ни регионов — проба «тупая» и о состоянии
// монитора ничего не знает. Config — monitors.config как есть; URL/хост
// проверяемого сервиса в нём — норма, проба его и должна дёрнуть.
type JobDTO struct {
	QueueID        int64           `json:"queue_id"`
	MonitorID      int64           `json:"monitor_id"`
	Kind           Kind            `json:"kind"`
	Config         json.RawMessage `json:"config"`
	TimeoutSeconds int             `json:"timeout_seconds"`
}

// NewJobDTO переводит задание из очереди в его сетевое представление.
func NewJobDTO(j Job) JobDTO {
	return JobDTO{
		QueueID:        j.QueueID,
		MonitorID:      j.MonitorID,
		Kind:           j.Monitor.Kind,
		Config:         j.Monitor.Config,
		TimeoutSeconds: j.Monitor.TimeoutSeconds,
	}
}

// Monitor собирает из задания минимальный Monitor для чекера — больше
// чекерам ничего не нужно (см. Checker.Check).
func (j JobDTO) Monitor() Monitor {
	return Monitor{
		ID:             j.MonitorID,
		Kind:           j.Kind,
		Config:         j.Config,
		TimeoutSeconds: j.TimeoutSeconds,
	}
}

// LeaseResponse — ответ POST /probe/lease.
type LeaseResponse struct {
	ProbeID int64    `json:"probe_id"`
	Region  string   `json:"region"`
	Jobs    []JobDTO `json:"jobs"`
}

// Timings — тайминги проверки в миллисекундах.
type Timings struct {
	DNS     uint32 `json:"dns"`
	Connect uint32 `json:"connect"`
	TLS     uint32 `json:"tls"`
	TTFB    uint32 `json:"ttfb"`
	Total   uint32 `json:"total"`
}

// ResultDTO — результат одной проверки, присланный пробой. Времени проверки
// здесь нет намеренно: timestamp ставит центр (time.Now().UTC() в момент
// приёма) — часам пробы центр не доверяет.
type ResultDTO struct {
	QueueID      int64      `json:"queue_id"`
	OK           bool       `json:"ok"`
	StatusCode   int        `json:"status_code,omitempty"`
	Error        string     `json:"error,omitempty"`
	Timings      Timings    `json:"timings"`
	BodySize     uint32     `json:"body_size,omitempty"`
	SSLExpiresAt *time.Time `json:"ssl_expires_at,omitempty"`
}

// NewResultDTO — сетевое представление результата чекера.
func NewResultDTO(queueID int64, r Result) ResultDTO {
	return ResultDTO{
		QueueID:    queueID,
		OK:         r.OK,
		StatusCode: r.StatusCode,
		Error:      r.Error,
		Timings: Timings{
			DNS: r.DNSMs, Connect: r.ConnectMs, TLS: r.TLSMs, TTFB: r.TTFBMs, Total: r.TotalMs,
		},
		BodySize:     r.BodySize,
		SSLExpiresAt: r.SSLExpiresAt,
	}
}

// Result восстанавливает Result из присланного пробой DTO.
func (r ResultDTO) Result() Result {
	return Result{
		OK:           r.OK,
		StatusCode:   r.StatusCode,
		Error:        r.Error,
		DNSMs:        r.Timings.DNS,
		ConnectMs:    r.Timings.Connect,
		TLSMs:        r.Timings.TLS,
		TTFBMs:       r.Timings.TTFB,
		TotalMs:      r.Timings.Total,
		BodySize:     r.BodySize,
		SSLExpiresAt: r.SSLExpiresAt,
	}
}

// ResultsRequest — тело POST /probe/results (пачка ≤ 100 результатов).
type ResultsRequest struct {
	Results []ResultDTO `json:"results"`
}

// ResultsResponse — ответ POST /probe/results: сколько результатов принято и
// сколько отвергнуто (задание чужое, lease истёк или задание уже выполнено).
type ResultsResponse struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}
