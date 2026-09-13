package escalation_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

type capturingEnqueuer struct {
	payloads []map[string]any
}

func (c *capturingEnqueuer) Enqueue(_ context.Context, _ int64, payload map[string]any) error {
	c.payloads = append(c.payloads, payload)
	return nil
}

func (c *capturingEnqueuer) EnqueueIdempotent(_ context.Context, _ int64, payload map[string]any, _ string) (bool, error) {
	c.payloads = append(c.payloads, payload)
	return true, nil
}

// Свойство сборки, не поведение конкретного отправителя: Extra не может
// подменить ни одно базовое поле, каким бы ключом оно ни было названо.
func TestDispatchBaseFieldsWinOverCollidingExtraKeys(t *testing.T) {
	enq := &capturingEnqueuer{}
	channels := []escalation.DispatchChannel{
		{ID: 1, Kind: "webhook", Target: "https://example.com/hook", Deliverable: true, AllowsDetails: true},
	}
	_, err := escalation.Dispatch(context.Background(),
		escalation.DispatchDeps{Outbox: enq, EmailEnabled: true, LogTag: "test"},
		escalation.DispatchInput{
			ProjectID: 42, Kind: testKind, Subject: "real subject", Body: "real body",
			URL: "https://real.example/url", Channels: channels,
			Extra: map[string]any{
				"kind":         "mutant_kind_from_extra",
				"url":          "https://evil.example/url",
				"project_id":   int64(999),
				"custom_field": "kept",
			},
		})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(enq.payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(enq.payloads))
	}
	p := enq.payloads[0]
	if p["kind"] != testKind {
		t.Errorf("kind = %v, want %q — Extra must not override a base field", p["kind"], testKind)
	}
	if p["url"] != "https://real.example/url" {
		t.Errorf("url = %v, want the base URL — Extra must not override it", p["url"])
	}
	if p["project_id"] != int64(42) {
		t.Errorf("project_id = %v, want 42 — Extra must not override it", p["project_id"])
	}
	if p["custom_field"] != "kept" {
		t.Errorf("custom_field lost: %+v", p)
	}
}

func dispatchOnceForLog(t *testing.T, extra map[string]any) string {
	t.Helper()
	buf := captureInfoLog(t)
	channels := []escalation.DispatchChannel{
		{ID: 1, Kind: "webhook", Target: "https://example.com/hook", Deliverable: true, AllowsDetails: true},
	}
	enq := &capturingEnqueuer{}
	_, err := escalation.Dispatch(context.Background(),
		escalation.DispatchDeps{Outbox: enq, EmailEnabled: true, LogTag: "test"},
		escalation.DispatchInput{
			ProjectID: 1, Kind: testKind, Subject: "s", Body: "b", URL: "https://x/y",
			Channels: channels, Extra: extra,
		})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	return buf.String()
}

// Обе стороны: предупреждение появляется ровно при столкновении и молчит,
// когда его нет — не хватало теста именно на журнал, не на итоговый payload.
func TestDispatchLogsExtraKeyCollisionOnlyWhenItHappens(t *testing.T) {
	t.Run("warns on collision", func(t *testing.T) {
		log := dispatchOnceForLog(t, map[string]any{"kind": "mutant"})
		if !strings.Contains(log, "Extra key collides with a base payload field") || !strings.Contains(log, "key=kind") {
			t.Errorf("expected a collision warning naming the key, got: %q", log)
		}
	})
	t.Run("silent without collision", func(t *testing.T) {
		log := dispatchOnceForLog(t, map[string]any{"custom_field": "safe"})
		if strings.Contains(log, "Extra key collides with a base payload field") {
			t.Errorf("unexpected collision warning without a collision: %q", log)
		}
	})
}
