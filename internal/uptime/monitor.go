// Package uptime — мониторы доступности (http/tcp/dns/heartbeat): типы,
// валидация и CRUD. Проверки и инциденты — в последующих задачах плана.
package uptime

import (
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

const (
	maxNameLen   = 200
	maxRegions   = 10
	maxRegionLen = 40
)

// Monitor — монитор доступности. Regions/ChannelIDs заполняются
// Service.Get/List; сам Monitor их не хранит в БД напрямую (см.
// monitor_regions/monitor_channels).
type Monitor struct {
	ID                 int64
	ProjectID          int64
	Name               string
	Kind               Kind
	Enabled            bool
	IntervalSeconds    int
	TimeoutSeconds     int
	Config             json.RawMessage // валидированный конфиг соответствующего типа
	FailThreshold      int
	RecoveryThreshold  int
	Consensus          Consensus
	RemindEveryMinutes int
	SSLAlertDays       int
	SSLExpiresAt       *time.Time
	// SSLAlertedDays — пороги (в днях), за которые уже отправлено
	// уведомление об истечении сертификата (monitors.ssl_alerted_days).
	// Заполняется только Service.SSLCandidates — остальные методы
	// (Get/List/...) его не читают и оставляют nil.
	SSLAlertedDays []int
	HeartbeatToken string // только для kind=heartbeat
	LastBeatAt     *time.Time
	CreatedAt      time.Time
	Regions        []string
	ChannelIDs     []int64
}

func validKind(k Kind) bool {
	switch k {
	case KindHTTP, KindTCP, KindDNS, KindHeartbeat:
		return true
	default:
		return false
	}
}

func validConsensus(c Consensus) bool {
	switch c {
	case ConsensusAny, ConsensusMajority, ConsensusAll:
		return true
	default:
		return false
	}
}

// validateMonitor проверяет общие поля монитора, регионы и (по kind)
// типизированный config. Каналы проверяются отдельно (checkChannelsBelongToProject) —
// это требует похода в БД внутри транзакции.
func validateMonitor(m Monitor, regions []string) error {
	if !validKind(m.Kind) {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidMonitor, m.Kind)
	}
	if m.Name == "" || utf8.RuneCountInString(m.Name) > maxNameLen {
		return fmt.Errorf("%w: name must be 1..%d characters", ErrInvalidMonitor, maxNameLen)
	}
	if m.IntervalSeconds < 30 {
		return fmt.Errorf("%w: interval_seconds must be >= 30", ErrInvalidMonitor)
	}
	if m.TimeoutSeconds < 1 || m.TimeoutSeconds > 120 {
		return fmt.Errorf("%w: timeout_seconds must be 1..120", ErrInvalidMonitor)
	}
	if m.TimeoutSeconds >= m.IntervalSeconds {
		return fmt.Errorf("%w: timeout_seconds must be less than interval_seconds", ErrInvalidMonitor)
	}
	if m.FailThreshold < 1 {
		return fmt.Errorf("%w: fail_threshold must be >= 1", ErrInvalidMonitor)
	}
	if m.RecoveryThreshold < 1 {
		return fmt.Errorf("%w: recovery_threshold must be >= 1", ErrInvalidMonitor)
	}
	if !validConsensus(m.Consensus) {
		return fmt.Errorf("%w: consensus must be any, majority or all", ErrInvalidMonitor)
	}
	if m.RemindEveryMinutes < 0 {
		return fmt.Errorf("%w: remind_every_minutes must be >= 0", ErrInvalidMonitor)
	}
	if m.SSLAlertDays < 0 {
		return fmt.Errorf("%w: ssl_alert_days must be >= 0", ErrInvalidMonitor)
	}
	if len(regions) > maxRegions {
		return fmt.Errorf("%w: at most %d regions", ErrInvalidMonitor, maxRegions)
	}
	for _, r := range regions {
		if r == "" || utf8.RuneCountInString(r) > maxRegionLen {
			return fmt.Errorf("%w: region names must be 1..%d characters", ErrInvalidMonitor, maxRegionLen)
		}
	}
	return validateConfig(m.Kind, m.Config)
}
