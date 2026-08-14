package ingest

import (
	"strconv"
	"testing"
	"time"
)

// TestCardinalityGuardCollapsesTail — сверх потолка значения схлопываются, а не
// теряются: суммарная нагрузка по проекту остаётся видна, пропадает лишь
// разбивка по хвосту.
func TestCardinalityGuardCollapsesTail(t *testing.T) {
	g := NewCardinalityGuard(3, time.Hour)

	// Первые три имени проходят как есть.
	for i := 0; i < 3; i++ {
		v := "GET /a" + strconv.Itoa(i)
		if got := g.Value(1, FieldTransaction, v); got != v {
			t.Fatalf("значение %d схлопнуто раньше потолка: %q", i, got)
		}
	}
	// Уже известное имя проходит и после исчерпания потолка — иначе честный
	// проект, набравший ровно потолок, потерял бы все свои эндпойнты.
	if got := g.Value(1, FieldTransaction, "GET /a0"); got != "GET /a0" {
		t.Fatalf("известное имя схлопнуто: %q", got)
	}
	// Новое — схлопывается.
	if got := g.Value(1, FieldTransaction, "GET /users/8812"); got != CardinalityOverflow {
		t.Fatalf("значение сверх потолка не схлопнуто: %q", got)
	}

	// Поля независимы: переполнение имён транзакций не режет окружения.
	if got := g.Value(1, FieldEnvironment, "production"); got != "production" {
		t.Fatalf("другое поле задето: %q", got)
	}
	// Проекты независимы.
	if got := g.Value(2, FieldTransaction, "GET /b"); got != "GET /b" {
		t.Fatalf("другой проект задет: %q", got)
	}
}

// TestCardinalityGuardCollapsesHostTail — host.name промоутируется в MetricPoint
// и обязан быть под тем же гардом, что имена транзакций и метрик: значение
// открыто клиенту (реальное имя хоста/пода), а в ClickHouse оно стоит в ключе
// точки — без потолка взрыв кардинальности будет тем же, что и у остальных
// полей под FieldHost.
func TestCardinalityGuardCollapsesHostTail(t *testing.T) {
	g := NewCardinalityGuard(2, time.Hour)

	if got := g.Value(1, FieldHost, "web-1"); got != "web-1" {
		t.Fatalf("значение в пределах потолка схлопнуто: %q", got)
	}
	if got := g.Value(1, FieldHost, "web-2"); got != "web-2" {
		t.Fatalf("значение в пределах потолка схлопнуто: %q", got)
	}
	// Известное имя проходит и после исчерпания потолка.
	if got := g.Value(1, FieldHost, "web-1"); got != "web-1" {
		t.Fatalf("известное имя схлопнуто: %q", got)
	}
	// Новое сверх потолка — схлопывается.
	if got := g.Value(1, FieldHost, "web-3"); got != CardinalityOverflow {
		t.Fatalf("значение сверх потолка не схлопнуто: %q", got)
	}
}

// TestCardinalityGuardReportsSamples — отчёт обязан нести ПРИМЕРЫ схлопнутых
// значений. Ради них он и существует: три имени подряд с разными числами
// объясняют причину («в имя попал идентификатор») мгновенно, а голый счётчик
// не объясняет ничего.
func TestCardinalityGuardReportsSamples(t *testing.T) {
	g := NewCardinalityGuard(2, time.Hour)
	g.Value(7, FieldTransaction, "GET /orders")
	g.Value(7, FieldTransaction, "POST /orders")
	for i := 0; i < 9; i++ {
		g.Value(7, FieldTransaction, "GET /users/"+strconv.Itoa(8812+i)+"/profile")
	}

	rep := g.Report(7)
	if len(rep) != 1 {
		t.Fatalf("полей в отчёте %d, want 1: %+v", len(rep), rep)
	}
	r := rep[0]
	if r.Field != FieldTransaction {
		t.Errorf("поле %q, want %q", r.Field, FieldTransaction)
	}
	if r.Collapsed != 9 {
		t.Errorf("схлопнуто %d, want 9", r.Collapsed)
	}
	if r.Distinct != 2 || r.Limit != 2 {
		t.Errorf("distinct=%d limit=%d, want 2/2", r.Distinct, r.Limit)
	}
	if len(r.Samples) == 0 {
		t.Fatal("в отчёте нет примеров — без них диагностировать нечем")
	}
	if len(r.Samples) > maxCardinalitySamples {
		t.Errorf("примеров %d, больше потолка %d", len(r.Samples), maxCardinalitySamples)
	}
	if r.Samples[0] != "GET /users/8812/profile" {
		t.Errorf("первый пример %q, ожидалось первое схлопнутое значение", r.Samples[0])
	}

	// Проект без переполнения в отчёт не попадает.
	if rep := g.Report(999); rep != nil {
		t.Errorf("отчёт для чистого проекта не пуст: %+v", rep)
	}
}

// TestCardinalityGuardWindowResets — проект, починивший имена, обязан вернуться
// к нормальной работе сам, без перезапуска инстанса.
func TestCardinalityGuardWindowResets(t *testing.T) {
	now := time.Unix(0, 0)
	g := NewCardinalityGuard(1, time.Minute)
	g.now = func() time.Time { return now }

	g.Value(3, FieldTransaction, "GET /a")
	if got := g.Value(3, FieldTransaction, "GET /b"); got != CardinalityOverflow {
		t.Fatalf("в пределах окна второе имя должно схлопнуться: %q", got)
	}

	now = now.Add(2 * time.Minute)
	if got := g.Value(3, FieldTransaction, "GET /b"); got != "GET /b" {
		t.Fatalf("после окна набор должен начаться заново: %q", got)
	}
	if rep := g.Report(3); rep != nil {
		t.Errorf("после смены окна отчёт должен обнулиться: %+v", rep)
	}
}

// TestCardinalityGuardDisabled — потолок 0 означает «выключено».
func TestCardinalityGuardDisabled(t *testing.T) {
	g := NewCardinalityGuard(0, time.Hour)
	for i := 0; i < 1000; i++ {
		v := "GET /" + strconv.Itoa(i)
		if got := g.Value(1, FieldTransaction, v); got != v {
			t.Fatalf("выключенный ограничитель схлопнул %q → %q", v, got)
		}
	}
	if g.Report(1) != nil {
		t.Error("выключенный ограничитель не должен ничего сообщать")
	}
	// nil-приёмник тоже безопасен: ограничитель может быть не сконфигурирован.
	var nilGuard *CardinalityGuard
	if got := nilGuard.Value(1, FieldTransaction, "x"); got != "x" {
		t.Errorf("nil-ограничитель изменил значение: %q", got)
	}
}

// TestCardinalityGuardBoundsProjects — карта проектов не растёт бесконечно, и
// вытеснение не снимает ограничение со всех разом (тот же дефект уже чинили в
// рейт-лимитере).
func TestCardinalityGuardBoundsProjects(t *testing.T) {
	g := NewCardinalityGuard(1, time.Hour)
	for id := int64(0); id < maxCardinalityProjects+500; id++ {
		g.Value(id, FieldTransaction, "GET /a")
	}
	g.mu.Lock()
	n := len(g.projects)
	g.mu.Unlock()
	if n > maxCardinalityProjects {
		t.Fatalf("проектов %d, потолок %d — граница не работает", n, maxCardinalityProjects)
	}
	if n < maxCardinalityProjects*8/10 {
		t.Fatalf("осталось %d проектов — похоже на полный сброс", n)
	}
}
