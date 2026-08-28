package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestHostOpenIncidentRowPlainNotSuppressed — открытый инцидент без detail и
// без подавления зависимостью: бейдж подавления и уточнение в скобках не
// рисуются, а ackControl (не-оператор) не показывает форму.
func TestHostOpenIncidentRowPlainNotSuppressed(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	suppressedText := i18n.T(ctx, "incident.badge.suppressed_by_dep")
	inc := host.Incident{ID: 1, Kind: "disk", Severity: "warning", CurrentValue: 93, StartedAt: time.Now()}
	out := renderTo(t, hostOpenIncidentRow(inc, 7, false, nil))
	if strings.Contains(out, suppressedText) {
		t.Fatalf("без SuppressedByDep бейдж подавления не должен появляться: %s", out)
	}
	if strings.Contains(out, "hint\">(") {
		t.Fatalf("без Detail не должно быть скобочного уточнения: %s", out)
	}
	if strings.Contains(out, "incident-ack-form") {
		t.Fatalf("не-оператор не должен видеть форму подтверждения: %s", out)
	}
}

// TestHostOpenIncidentRowSuppressedWithDetail — подавленный зависимостью
// инцидент с detail: оба опциональных блока присутствуют, и оператор видит
// кнопку подтверждения.
func TestHostOpenIncidentRowSuppressedWithDetail(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	suppressedText := i18n.T(ctx, "incident.badge.suppressed_by_dep")
	inc := host.Incident{ID: 2, Kind: "load", Severity: "critical", CurrentValue: 4.2,
		Detail: "load avg 4.2 above threshold", StartedAt: time.Now(), SuppressedByDep: true}
	out := renderTo(t, hostOpenIncidentRow(inc, 7, true, nil))
	if !strings.Contains(out, suppressedText) {
		t.Fatalf("с SuppressedByDep должен быть бейдж подавления %q: %s", suppressedText, out)
	}
	if !strings.Contains(out, "load avg 4.2 above threshold") {
		t.Fatalf("с Detail должно быть скобочное уточнение: %s", out)
	}
	if !strings.Contains(out, "incident-ack-form") {
		t.Fatalf("оператор без подтверждения должен видеть форму: %s", out)
	}
}

// TestHostOpenIncidentRowAckedByEmailResolved — AcknowledgedBy резолвится в
// email через батч-карту ackedBy (W2-C находка 4): ackControl должен
// показать email, а не только время.
func TestHostOpenIncidentRowAckedByEmailResolved(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	uid := int64(9)
	inc := host.Incident{ID: 3, Kind: "memory", Severity: "warning", CurrentValue: 80,
		StartedAt: time.Now(), AcknowledgedAt: &at, AcknowledgedBy: &uid}
	out := renderTo(t, hostOpenIncidentRow(inc, 7, true, map[int64]string{9: "op@example.com"}))
	if !strings.Contains(out, "op@example.com") {
		t.Fatalf("email подтвердившего должен резолвиться через ackedBy: %s", out)
	}
}

// TestHostIncidentRowSuppressedByDep — историческая строка (таблица
// инцидентов хоста) с подавлением зависимостью несёт бейдж рядом со статусом.
func TestHostIncidentRowSuppressedByDep(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	suppressedText := i18n.T(ctx, "incident.badge.suppressed_by_dep")
	inc := host.Incident{Kind: "disk", Status: "resolved", Severity: "warning", PeakValue: 95, CurrentValue: 40, StartedAt: time.Now(), SuppressedByDep: true}
	out := renderTo(t, hostIncidentRow(inc))
	if !strings.Contains(out, suppressedText) {
		t.Fatalf("подавленная историческая строка должна нести бейдж %q: %s", suppressedText, out)
	}
}

// TestHostIncidentRowNotSuppressed — обратная ветка: без SuppressedByDep
// доп. бейджа рядом со статусом нет (остаётся только бейдж статуса).
func TestHostIncidentRowNotSuppressed(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	suppressedText := i18n.T(ctx, "incident.badge.suppressed_by_dep")
	inc := host.Incident{Kind: "disk", Status: "resolved", Severity: "warning", PeakValue: 95, CurrentValue: 40, StartedAt: time.Now()}
	out := renderTo(t, hostIncidentRow(inc))
	if strings.Contains(out, suppressedText) {
		t.Fatalf("без SuppressedByDep бейдж подавления не должен появляться: %s", out)
	}
}
