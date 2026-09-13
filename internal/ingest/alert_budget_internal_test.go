package ingest

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

// Задерживает Upsert и запоминает остаток бюджета ctx у вызова Get — ловит,
// не голодает ли постановка алерта от того, сколько съел Upsert.
type slowUpsertIssueSvc struct {
	delay             time.Duration
	res               issue.UpsertResult
	getCtxRemaining   time.Duration
	getCalledWithNoDL bool
}

func (s *slowUpsertIssueSvc) Upsert(_ context.Context, _ int64, _, _, _, _, _ string, _ time.Time) (issue.UpsertResult, error) {
	time.Sleep(s.delay)
	return s.res, nil
}

func (s *slowUpsertIssueSvc) Get(ctx context.Context, id int64) (issue.Issue, error) {
	if dl, ok := ctx.Deadline(); ok {
		s.getCtxRemaining = time.Until(dl)
	} else {
		s.getCalledWithNoDL = true
	}
	return issue.Issue{ID: id, TimesSeen: 1}, nil
}

type deadlineCapturingAlertSink struct {
	called    bool
	remaining time.Duration
}

func (c *deadlineCapturingAlertSink) OnIssue(ctx context.Context, _ alert.Event) {
	c.called = true
	if dl, ok := ctx.Deadline(); ok {
		c.remaining = time.Until(dl)
	}
}

// Get/OnIssue делили один ctx с Upsert — просадка PG на upsert молча теряла
// алерт (событие уже записано, найти пропажу нечем).
func TestPipelineAlertGetsFreshBudgetAfterSlowUpsert(t *testing.T) {
	const upsertDelay = 2 * time.Second

	issues := &slowUpsertIssueSvc{delay: upsertDelay, res: issue.UpsertResult{IssueID: 1, New: true}}
	sink := &deadlineCapturingAlertSink{}
	p := NewPipeline(nil, nil)
	p.issues = issues
	p.batcher = &fakeBatcher{}
	p.Alerts = sink
	p.Scrub = nil

	ev := &ParsedEvent{EventID: "e1", Timestamp: time.Now().UTC(), Title: "boom"}
	p.process(task{projectID: 1, orgID: 1, ev: ev})

	if !sink.called {
		t.Fatal("OnIssue не вызван")
	}
	if issues.getCalledWithNoDL {
		t.Fatal("Get вызван с ctx без дедлайна")
	}
	// Общий ctx после 2с сна на Upsert оставил бы ~3с из 5с; свежий даёт ~5с.
	// Порог 4с разводит оба случая с большим запасом на дрожание шедулера.
	if issues.getCtxRemaining < 4*time.Second {
		t.Errorf("Get: остаток бюджета = %v, want >= 4s (не должен зависеть от времени, съеденного Upsert)", issues.getCtxRemaining)
	}
	if sink.remaining < 4*time.Second {
		t.Errorf("OnIssue: остаток бюджета = %v, want >= 4s (не должен зависеть от времени, съеденного Upsert)", sink.remaining)
	}
}
