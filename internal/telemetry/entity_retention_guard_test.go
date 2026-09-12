package telemetry

import (
	"testing"
	"time"
)

func TestEntityRulesDeclareRetention(t *testing.T) {
	const minRules = 6
	if len(entityRules) < minRules {
		t.Fatalf("правил в entityRules %d, ожидалось не меньше %d: перечисление усечено, сторож ничего не проверяет",
			len(entityRules), minRules)
	}
	all := Retentions{
		Events:      time.Hour,
		Metrics:     time.Hour,
		Profiles:    time.Hour,
		Incidents:   time.Hour,
		Deployments: time.Hour,
	}
	for _, rule := range entityRules {
		if all.forKind(rule.retention) <= 0 {
			t.Errorf("правило %s не называет класс срока хранения явно: оно молча унаследует чужой срок или не выполнится вовсе",
				rule.table)
		}
	}
}

func TestRetentionsAnyRequiresPositive(t *testing.T) {
	if (Retentions{}).Any() {
		t.Error("пустые сроки: Any() = true — чистильщик запустится и удалит всё")
	}
	if !(Retentions{Profiles: time.Hour}).Any() {
		t.Error("задан срок профилей: Any() = false — правило не выполнится")
	}
}

func TestRetentionsForKindUnsetIsZero(t *testing.T) {
	all := Retentions{Events: time.Hour, Metrics: time.Hour, Profiles: time.Hour, Incidents: time.Hour, Deployments: time.Hour}
	if d := all.forKind(retentionUnset); d != 0 {
		t.Errorf("forKind(retentionUnset) = %v, want 0: правило без срока унаследовало бы чужой", d)
	}
}
