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

const defaultSpikeInterval = time.Minute

// Троттлинг внутри Evaluator делает повторные срабатывания на каждом тике
// безопасными.
type Spike struct {
	Svc       *Service
	Outbox    *notify.Outbox
	Issues    *issue.Service
	Events    *event.Query
	Evaluator *Evaluator

	// По умолчанию defaultSpikeInterval (1 минута).
	Interval time.Duration
}

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

		// Два запроса на правило, не запрос на каждую активную группу — иначе
		// 10 тысяч групп дали бы 10 тысяч ClickHouse round-trip'ов в минуту.
		counts, err := s.Events.CountsSince(ctx, rule.ProjectID, since, uint64(rule.Threshold))
		if err != nil {
			slog.Error("alert spike: counts since failed", "project_id", rule.ProjectID, "error", err)
			continue
		}
		if len(counts) == 0 {
			continue
		}

		ids := make([]int64, 0, len(counts))
		for id := range counts {
			ids = append(ids, id)
		}
		// Только группы, перешагнувшие порог — не все активные группы проекта.
		issues, err := s.Issues.ByIDs(ctx, rule.ProjectID, ids)
		if err != nil {
			slog.Error("alert spike: issues lookup failed", "project_id", rule.ProjectID, "error", err)
			continue
		}

		for _, iss := range issues {
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

// Все проекты разом — Spike.Run не должен опрашивать их по одному.
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
