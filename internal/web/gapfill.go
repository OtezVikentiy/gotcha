package web

import "time"

// Сетка выровнена к границам step от эпохи, как toStartOfInterval в ClickHouse — иначе ключи
// корзин не совпали бы с теми, что вернул запрос (тот же step передаётся и в запрос, и сюда).
func fillSeries[T any](src []T, from, to time.Time, step time.Duration,
	at func(T) time.Time, gap func(time.Time) T) []T {
	if step <= 0 || !from.Before(to) {
		return src
	}
	start := truncStepEpoch(from, step)
	// Страховка от абсурдной сетки при кривом step.
	n := int(to.Sub(start)/step) + 1
	if n < 1 || n > 5000 {
		return src
	}
	idx := make(map[int64]T, len(src))
	for _, p := range src {
		idx[truncStepEpoch(at(p), step).UnixNano()] = p
	}
	out := make([]T, 0, n)
	for t := start; !t.After(to); t = t.Add(step) {
		if p, ok := idx[t.UnixNano()]; ok {
			out = append(out, p)
		} else {
			out = append(out, gap(t))
		}
	}
	return out
}

// Округление к эпохе (как toStartOfInterval), не к нулю Go — иначе ключи сетки разошлись
// бы с корзинами запроса для шагов, не делящих сутки нацело.
func truncStepEpoch(t time.Time, step time.Duration) time.Time {
	if step <= 0 {
		return t.UTC()
	}
	off := t.UnixNano() % int64(step)
	if off < 0 {
		off += int64(step)
	}
	return time.Unix(0, t.UnixNano()-off).UTC()
}
