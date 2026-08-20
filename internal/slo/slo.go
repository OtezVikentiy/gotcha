// Package slo реализует SLO/SLI + error budget: определения целей качества,
// инциденты сжигания бюджета (burn rate) и связанную математику/оценщик.
//
// ЛОВУШКА: internal/alert/budget.go — это rate-limit УВЕДОМЛЕНИЙ, а НЕ error
// budget. К этому пакету он отношения не имеет.
package slo

import "time"

// SLIKind — тип индикатора уровня сервиса (SLI), на котором строится SLO.
type SLIKind string

const (
	// SLIAvailability — доля успешных транзакций (good/total) за окно.
	SLIAvailability SLIKind = "availability"
	// SLILatency — доля транзакций быстрее порога ThresholdMS.
	SLILatency SLIKind = "latency"
	// SLIUptime — доля успешных проверок uptime-монитора.
	SLIUptime SLIKind = "uptime"
)

// SLO — определение цели уровня сервиса на скользящем окне WindowDays.
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

// Incident — открытый или закрытый инцидент сжигания бюджета SLO.
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
}
