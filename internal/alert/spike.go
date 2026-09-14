package alert

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

const defaultSpikeInterval = time.Minute

// Дедлайн тика — доля Interval, но не меньше пола: та же защита от
// зависшего прохода, что у escalation.Scheduler.
const (
	spikeTickBudgetShare = 0.8
	minSpikeTickBudget   = 10 * time.Second
)

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

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits
	skipped         atomic.Int64
}

// Self-метрика живости: остановленный или зависший цикл спайков снаружи
// неотличим от «всплесков не было».
func (s *Spike) LastTickUnix() int64 { return s.lastTickUnix.Load() }

func (s *Spike) LastTickSeconds() float64 {
	return math.Float64frombits(s.lastTickSeconds.Load())
}

func (s *Spike) LastTickSkippedRules() int64 { return s.skipped.Load() }

func (s *Spike) effectiveInterval() time.Duration {
	if s.Interval <= 0 {
		return defaultSpikeInterval
	}
	return s.Interval
}

// Считается от effectiveInterval, не от сырого Interval — иначе прод (Interval
// не задан) получал бы minSpikeTickBudget вместо ~48 секунд.
func (s *Spike) tickBudget() time.Duration {
	budget := time.Duration(float64(s.effectiveInterval()) * spikeTickBudgetShare)
	if budget < minSpikeTickBudget {
		return minSpikeTickBudget
	}
	return budget
}

func (s *Spike) Run(ctx context.Context) {
	ticker := time.NewTicker(s.effectiveInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Экспортирован ради теста — цикл Run проверять неудобно.
func (s *Spike) Tick(ctx context.Context) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, s.tickBudget())
	defer cancel()
	defer func() {
		s.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
		if ctx.Err() != nil {
			return
		}
		s.lastTickUnix.Store(time.Now().Unix())
	}()

	rules, err := s.Svc.SpikeRules(ctx)
	if err != nil {
		slog.Error("alert spike: rules lookup failed", "error", err)
		return
	}

	now := time.Now()
	done := len(rules)
	for i, rule := range rules {
		if ctx.Err() != nil {
			slog.Warn("alert spike: tick budget exhausted, remaining rules skipped",
				"skipped_rules", len(rules)-i, "budget", s.tickBudget())
			done = i
			break
		}
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
	s.skipped.Store(int64(len(rules) - done))
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
