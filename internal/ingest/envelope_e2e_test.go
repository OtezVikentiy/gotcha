package ingest_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// eventJSONWithID переиспользует testEventJSON с другим event_id — иначе два
// события в одном тесте группируются в один и тот же issue по одинаковому стеку.
func eventJSONWithID(id string) string {
	return strings.Replace(testEventJSON, "9ec79c33ec9942ab8353589fcb2e04dc", id, 1)
}

func threeSignalEnvelope(eventID, transactionJSON string) string {
	return "{}\n" +
		"{\"type\":\"event\"}\n" + strings.ReplaceAll(eventJSONWithID(eventID), "\n", "") + "\n" +
		"{\"type\":\"transaction\"}\n" + strings.ReplaceAll(transactionJSON, "\n", "") + "\n" +
		sentryProfileEnvelopeItem(5)
}

func TestEnvelopeThreeSignalsScopedByKeyKind(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	keys, err := s.orgSvc.CreateKeys(ctx, s.project.ID, org.KindBrowser, org.KindServer)
	if err != nil {
		t.Fatalf("CreateKeys: %v", err)
	}
	browserKey, serverKey := keys[0], keys[1]
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)
	pid := uint64(s.project.ID)

	rejectedBefore := s.h.RejectedBy(ingest.RejectKeyScope, ingest.SignalProfile)
	resp := s.post(t, path,
		threeSignalEnvelope(strings.Repeat("1", 32), freshTransactionJSON()), false, browserKey.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("browser key: status = %d, want 200", resp.StatusCode)
	}
	if got := waitCH(t, s, "SELECT count(*) FROM events WHERE project_id = ?", []any{pid}, 1); got != 1 {
		t.Fatalf("browser key: events rows = %d, want 1", got)
	}
	if got := waitCH(t, s, "SELECT count(*) FROM transactions WHERE project_id = ?", []any{pid}, 1); got != 1 {
		t.Fatalf("browser key: transactions rows = %d, want 1", got)
	}
	if got := s.profiles.count(); got != 0 {
		t.Fatalf("browser key: profile sink = %d, want 0 — profile вне скоупа не должен доехать", got)
	}
	if got := s.h.RejectedBy(ingest.RejectKeyScope, ingest.SignalProfile) - rejectedBefore; got != 1 {
		t.Fatalf("browser key: RejectedBy(key_scope, profile) += %d, want 1 — profile обязан быть отброшен именно по скоупу, а не потерян иначе", got)
	}

	resp = s.post(t, path,
		threeSignalEnvelope(strings.Repeat("2", 32), freshTransactionJSON()), false, serverKey.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("server key: status = %d, want 200", resp.StatusCode)
	}
	if got := waitCH(t, s, "SELECT count(*) FROM events WHERE project_id = ?", []any{pid}, 2); got != 2 {
		t.Fatalf("server key: events rows = %d, want 2", got)
	}
	if got := waitCH(t, s, "SELECT count(*) FROM transactions WHERE project_id = ?", []any{pid}, 2); got != 2 {
		t.Fatalf("server key: transactions rows = %d, want 2", got)
	}
	if got := s.profiles.count(); got != 1 {
		t.Fatalf("server key: profile sink = %d, want 1 — серверный ключ обязан пропустить profile", got)
	}
	if got := s.h.RejectedBy(ingest.RejectKeyScope, ingest.SignalProfile) - rejectedBefore; got != 1 {
		t.Fatalf("server key: RejectedBy(key_scope, profile) += %d от начала теста, want 1 — второй запрос не должен отбраковывать profile", got)
	}
}
