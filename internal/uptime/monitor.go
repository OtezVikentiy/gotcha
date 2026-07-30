// Package uptime — мониторы доступности (http/tcp/dns/heartbeat): типы,
// валидация и CRUD. Проверки и инциденты — в последующих задачах плана.
package uptime

import (
	"encoding/json"
	"strconv"
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
	ID                int64
	ProjectID         int64
	Name              string
	Kind              Kind
	Enabled           bool
	IntervalSeconds   int
	TimeoutSeconds    int
	Config            json.RawMessage // валидированный конфиг соответствующего типа
	FailThreshold     int
	RecoveryThreshold int
	// Retries — сколько РАЗ повторить одну проверку при неуспехе, прежде чем
	// записать её как сбой (0 = без повторов). Гасит транзиентные блипы
	// (например периодический TLS-тарпит фронта, проходящий на повторе) — в
	// отличие от FailThreshold, который считает уже записанные сбои подряд.
	Retries            int
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

	// RegionCount — сколько регионов НАСТРОЕНО у монитора. Отдельно от Regions,
	// потому что путь детекции (lease → scanMonitor) список регионов не грузит, а
	// консенсусу all/majority нужен именно знаменатель: без него «все регионы
	// down» считалось по регионам, которые УЖЕ прислали результат, и у свежего
	// монитора на 3 региона первый же упавший регион давал down==decided==1,
	// то есть срабатывал и `all`, и `majority`. 0 — счёт неизвестен, тогда
	// aggregate откатывается на прежнее поведение (см. detector.aggregate).
	RegionCount int
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
		return invalid("kind", "unknown_kind", "kind", string(m.Kind))
	}
	if m.Name == "" || utf8.RuneCountInString(m.Name) > maxNameLen {
		return invalid("name", "name_length", "max", strconv.Itoa(maxNameLen))
	}
	if m.IntervalSeconds < 30 {
		return invalid("interval_seconds", "interval_min", "min", "30")
	}
	if m.TimeoutSeconds < 1 || m.TimeoutSeconds > 120 {
		return invalid("timeout_seconds", "timeout_range")
	}
	if m.TimeoutSeconds >= m.IntervalSeconds {
		return invalid("timeout_seconds", "timeout_vs_interval")
	}
	if m.FailThreshold < 1 {
		return invalid("fail_threshold", "fail_threshold_min")
	}
	if m.RecoveryThreshold < 1 {
		return invalid("recovery_threshold", "recovery_threshold_min")
	}
	if m.Retries < 0 || m.Retries > 10 {
		return invalid("retries", "retries_range")
	}
	if !validConsensus(m.Consensus) {
		return invalid("consensus", "consensus_invalid")
	}
	if m.RemindEveryMinutes < 0 {
		return invalid("remind_every_minutes", "remind_min")
	}
	if m.SSLAlertDays < 0 {
		return invalid("ssl_alert_days", "ssl_days_min")
	}
	if len(regions) > maxRegions {
		return invalid("regions", "regions_max", "max", strconv.Itoa(maxRegions))
	}
	for _, r := range regions {
		if r == "" || utf8.RuneCountInString(r) > maxRegionLen {
			return invalid("regions", "region_length", "max", strconv.Itoa(maxRegionLen))
		}
	}
	return validateConfig(m.Kind, m.Config)
}
