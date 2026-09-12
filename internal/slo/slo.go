package slo

import "time"

type SLIKind string

const (
	SLIAvailability SLIKind = "availability"
	SLILatency      SLIKind = "latency"
	SLIUptime       SLIKind = "uptime"
)

type SLO struct {
	ID          int64
	ProjectID   int64
	Name        string
	Kind        SLIKind
	Target      float64 // цель ∈ (0,1), напр. 0.99
	WindowDays  int     // скользящее окно бюджета, 1..90
	Transaction string  // фильтр транзакции (availability/latency), "" → любая
	Environment string  // фильтр окружения, "" → любое
	ThresholdMS int     // порог задержки в мс (latency)
	MonitorID   *int64  // монитор uptime (uptime), nullable

	BurnThreshold float64 // множитель burn rate для алерта (напр. 14.4)
	BurnLongMin   int     // длинное окно burn rate, минуты
	BurnShortMin  int     // короткое окно burn rate, минуты

	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Incident struct {
	ID              int64
	SLOID           int64
	ProjectID       int64
	Status          string  // 'open' | 'resolved'
	BurnRate        float64 // burn rate на момент открытия/последнего обновления
	BudgetRemaining *float64
	StartedAt       time.Time
	ResolvedAt      *time.Time
	InMaintenance   bool
	NotifiedOpen    bool
	NotifiedClose   bool
	AcknowledgedAt  *time.Time
	AcknowledgedBy  *int64
	Severity        string
}
