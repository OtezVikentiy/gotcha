package web

import "time"

// fillSeries вставляет пропущенные корзины ряда на регулярной сетке [from,to]
// с шагом step, чтобы график покрывал ВЫБРАННОЕ окно целиком, а не только
// диапазон, где есть данные. Пустые корзины рисуются разрывом (линии) или
// нулём (столбики), и ось X идёт по всему окну — поведение Grafana/
// Elasticsearch: выбрал 30 дней → видишь 30 дней, даже если данные лишь за
// последние сутки.
//
// Сетка выровнена к границам step от эпохи — так же, как toStartOfInterval в
// ClickHouse, поэтому ключи корзин совпадают с теми, что вернул запрос (тот же
// step передаётся и в запрос, и сюда). at(p) — момент корзины точки; gap(t) —
// «пустая» корзина момента t (нулевой счётчик либо NaN, по типу точки).
func fillSeries[T any](src []T, from, to time.Time, step time.Duration,
	at func(T) time.Time, gap func(time.Time) T) []T {
	if step <= 0 || !from.Before(to) {
		return src
	}
	start := truncStepEpoch(from, step)
	// Число корзин ограничено целевым (~48–120 у autoStep/perfBucketStep), но
	// на всякий случай страхуемся от абсурдной сетки при кривом step.
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

// truncStepEpoch округляет момент вниз к границе корзины step, выровненной к
// unix-эпохе (как toStartOfInterval), а не к нулевому времени Go — иначе ключи
// сетки не совпали бы с корзинами запроса для шагов, не делящих сутки нацело.
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
