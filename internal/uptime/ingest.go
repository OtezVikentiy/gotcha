package uptime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// один и тот же путь обрабатывает результат и локальной, и выносной пробы —
// детекция инцидентов и запись в ClickHouse существуют в одном экземпляре.
type Ingestor struct {
	Svc      *Service
	Writer   *ResultWriter                                                           // может быть nil — тогда без записи в CH
	OnResult func(ctx context.Context, m Monitor, region string, r Result, st State) // nil — детекция не запускается
}

// claim идёт первым и монопольным: неудачный claim (уже применено или lease
// истёк) молча отбрасывает результат — иначе ApplyResult сработал бы дважды.
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
	return i.AcceptClaimed(ctx, j, at, r)
}

// вызывающий уже изъял задание из очереди (пачка POST /probe/results) — здесь
// claim не повторяется, гарантия «ровно один раз» лежит на нём.
func (i *Ingestor) AcceptClaimed(ctx context.Context, j Job, at time.Time, r Result) error {
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

// эти DTO используют обе стороны — серверные ручки /probe/lease и
// /probe/results и клиент выносной пробы — формат должен быть общим.

// Limit ≤ 0 значит «сколько дашь» — сервер подставит свой дефолт и обрежет
// по своему максимуму.
type LeaseRequest struct {
	Limit int `json:"limit"`
}

// из задания берётся только то, что нужно чекеру — ни project_id, ни
// порогов, ни регионов: проба ничего не знает о состоянии монитора.
type JobDTO struct {
	QueueID        int64           `json:"queue_id"`
	MonitorID      int64           `json:"monitor_id"`
	Kind           Kind            `json:"kind"`
	Config         json.RawMessage `json:"config"`
	TimeoutSeconds int             `json:"timeout_seconds"`
	Retries        int             `json:"retries"` // повтор делает сама проба, поэтому едет с заданием
}

func NewJobDTO(j Job) JobDTO {
	return JobDTO{
		QueueID:        j.QueueID,
		MonitorID:      j.MonitorID,
		Kind:           j.Monitor.Kind,
		Config:         j.Monitor.Config,
		TimeoutSeconds: j.Monitor.TimeoutSeconds,
		Retries:        j.Monitor.Retries,
	}
}

func (j JobDTO) Monitor() Monitor {
	return Monitor{
		ID:             j.MonitorID,
		Kind:           j.Kind,
		Config:         j.Config,
		TimeoutSeconds: j.TimeoutSeconds,
		Retries:        j.Retries,
	}
}

type LeaseResponse struct {
	ProbeID int64    `json:"probe_id"`
	Region  string   `json:"region"`
	Jobs    []JobDTO `json:"jobs"`
}

// миллисекунды.
type Timings struct {
	DNS     uint32 `json:"dns"`
	Connect uint32 `json:"connect"`
	TLS     uint32 `json:"tls"`
	TTFB    uint32 `json:"ttfb"`
	Total   uint32 `json:"total"`
}

// timestamp здесь нет намеренно — его ставит центр при приёме, часам пробы
// он не доверяет.
type ResultDTO struct {
	QueueID      int64      `json:"queue_id"`
	OK           bool       `json:"ok"`
	StatusCode   int        `json:"status_code,omitempty"`
	Error        string     `json:"error,omitempty"`
	Timings      Timings    `json:"timings"`
	BodySize     uint32     `json:"body_size,omitempty"`
	SSLExpiresAt *time.Time `json:"ssl_expires_at,omitempty"`
}

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

// пачка ≤ 100 результатов.
type ResultsRequest struct {
	Results []ResultDTO `json:"results"`
}

// Dropped — задание валидно, но применить успел кто-то другой раньше.
type ResultsResponse struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Dropped  int `json:"dropped"`
}
