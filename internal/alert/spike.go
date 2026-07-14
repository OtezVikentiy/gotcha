package alert

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// defaultSpikeInterval — период тика Spike, если Spike.Interval не задан.
const defaultSpikeInterval = time.Minute

// Spike — периодически сканирует все enabled spike-правила: для issue,
// активных в окне правила, считает число событий за это же окно в CH и, если
// оно достигает порога, зовёт Evaluator.OnIssue(kind=spike). Троттлинг
// внутри Evaluator делает повторные срабатывания на каждом тике безопасными.
type Spike struct {
	Svc       *Service
	Outbox    *notify.Outbox
	Issues    *issue.Service
	Events    *event.Query
	Evaluator *Evaluator

	// Interval — период тика. По умолчанию defaultSpikeInterval (1 минута).
	Interval time.Duration
}

// Run тикает с Spike.Interval, на каждом тике сканирует все spike-правила.
// Возвращается, когда ctx отменяется.
func (s *Spike) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultSpikeInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Spike) tick(ctx context.Context) {
	rules, err := s.Svc.SpikeRules(ctx)
	if err != nil {
		slog.Error("alert spike: rules lookup failed", "error", err)
		return
	}

	now := time.Now()
	for _, rule := range rules {
		since := now.Add(-time.Duration(rule.WindowMinutes) * time.Minute)

		issues, err := s.Issues.ActiveSince(ctx, rule.ProjectID, since)
		if err != nil {
			slog.Error("alert spike: active issues lookup failed", "project_id", rule.ProjectID, "error", err)
			continue
		}

		for _, iss := range issues {
			count, err := s.Events.CountSince(ctx, rule.ProjectID, iss.ID, since)
			if err != nil {
				slog.Error("alert spike: count since failed",
					"project_id", rule.ProjectID, "issue_id", iss.ID, "error", err)
				continue
			}
			if count < uint64(rule.Threshold) {
				continue
			}
			s.Evaluator.OnIssue(ctx, Event{
				ProjectID: rule.ProjectID,
				IssueID:   iss.ID,
				Kind:      KindSpike,
				Title:     iss.Title,
				Culprit:   iss.Culprit,
				Level:     iss.Level,
				TimesSeen: iss.TimesSeen,
			})
		}
	}
}

// SpikeRules возвращает все включённые spike-правила всех проектов —
// используется Spike.Run на каждом тике, чтобы не опрашивать проекты по
// одному.
func (s *Service) SpikeRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, kind, enabled, threshold, window_minutes, throttle_minutes
		FROM alert_rules WHERE kind = $1 AND enabled = true`, KindSpike)
	if err != nil {
		return nil, fmt.Errorf("alert: spike rules: %w", err)
	}
	defer rows.Close()

	var out []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Kind, &r.Enabled,
			&r.Threshold, &r.WindowMinutes, &r.ThrottleMinutes); err != nil {
			return nil, fmt.Errorf("alert: spike rules: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
