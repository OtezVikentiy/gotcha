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
	// повторы одной проверки (0 — без них), в отличие от FailThreshold,
	// который считает уже записанные сбои подряд.
	Retries            int
	Consensus          Consensus
	RemindEveryMinutes int
	SSLAlertDays       int
	SSLExpiresAt       *time.Time
	// заполняется только Service.SSLCandidates — остальные методы
	// (Get/List/...) оставляют nil.
	SSLAlertedDays []int
	HeartbeatToken string // только для kind=heartbeat
	LastBeatAt     *time.Time
	CreatedAt      time.Time
	Regions        []string
	ChannelIDs     []int64

	// знаменатель для aggregate (all/majority) — без него «все down»
	// считалось бы только по уже ответившим регионам. 0 — счёт неизвестен.
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

// каналы проверяются отдельно (checkChannelsBelongToProject) — это требует
// похода в БД внутри транзакции.
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
