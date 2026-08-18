package telemetry

import (
	"testing"
	"time"
)

// TestEntityRulesDeclareRetention — сторож класса «правило без явного срока».
//
// Правила чистильщика перечислимы, поэтому класс закрывается инструментом, а не
// внимательностью. Находка №108 возникла именно так: правило добавили, а срок
// ему достался общий по умолчанию — и регрессия профиля переживала свои сэмплы
// на восемьдесят три дня, открываясь пустой карточкой.
//
// Нижняя граница числа правил обязательна: пустое или усечённое перечисление
// означало бы, что сторож ослеп, а не что нарушений нет.
func TestEntityRulesDeclareRetention(t *testing.T) {
	const minRules = 6
	if len(entityRules) < minRules {
		t.Fatalf("правил в entityRules %d, ожидалось не меньше %d: перечисление усечено, сторож ничего не проверяет",
			len(entityRules), minRules)
	}
	// Все сроки заданы и положительны — значит, ноль на выходе означает ровно
	// одно: у правила не проставлен retention (retentionUnset).
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

// TestRetentionsAnyRequiresPositive — Any() решает, запускать ли чистильщика
// вообще. Нулевые сроки означают «не удалять ничего», и проход, стартовавший
// при них, удалил бы всё, что старше нуля секунд.
func TestRetentionsAnyRequiresPositive(t *testing.T) {
	if (Retentions{}).Any() {
		t.Error("пустые сроки: Any() = true — чистильщик запустится и удалит всё")
	}
	if !(Retentions{Profiles: time.Hour}).Any() {
		t.Error("задан срок профилей: Any() = false — правило не выполнится")
	}
}

// TestRetentionsForKindUnsetIsZero — нулевое значение retentionKind не должно
// разрешать удаление: это единственное, на чём держится сторож выше.
func TestRetentionsForKindUnsetIsZero(t *testing.T) {
	all := Retentions{Events: time.Hour, Metrics: time.Hour, Profiles: time.Hour, Incidents: time.Hour, Deployments: time.Hour}
	if d := all.forKind(retentionUnset); d != 0 {
		t.Errorf("forKind(retentionUnset) = %v, want 0: правило без срока унаследовало бы чужой", d)
	}
}
