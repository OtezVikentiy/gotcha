package agent

import (
	"errors"
	"testing"
	"time"
)

// TestThrottledProcsCachesWithinInterval: реальная проба вызывается один раз
// на первое обращение и не чаще interval дальше — между вызовами отдаётся
// последний снимок (ops-MED: проба процессов — самая дорогая часть тика).
func TestThrottledProcsCachesWithinInterval(t *testing.T) {
	calls := 0
	real := func() (map[string]int, error) {
		calls++
		return map[string]int{"running": calls}, nil
	}
	now := time.Unix(1000, 0)
	throttled := throttledProcs(real, func() time.Time { return now }, 60*time.Second)

	v1, err := throttled()
	if err != nil || calls != 1 || v1["running"] != 1 {
		t.Fatalf("первый вызов: v=%v err=%v calls=%d, хочу v[running]=1 err=nil calls=1", v1, err, calls)
	}

	now = now.Add(30 * time.Second) // внутри interval — реального опроса быть не должно
	v2, err := throttled()
	if err != nil || calls != 1 {
		t.Fatalf("второй вызов (внутри interval): err=%v calls=%d, хочу err=nil calls=1 (кэш)", err, calls)
	}
	if v2["running"] != 1 {
		t.Fatalf("v2 = %v, хочу кэш v1 (running=1)", v2)
	}

	now = now.Add(31 * time.Second) // суммарно 61с — за interval, новый реальный опрос
	v3, err := throttled()
	if err != nil || calls != 2 {
		t.Fatalf("третий вызов (за interval): err=%v calls=%d, хочу err=nil calls=2 (свежий опрос)", err, calls)
	}
	if v3["running"] != 2 {
		t.Fatalf("v3 = %v, хочу свежий снимок (running=2)", v3)
	}
}

// TestThrottledProcsCachesError: ошибка боевой пробы тоже кэшируется — не
// дёргаем её повторно только чтобы получить ту же ошибку раньше срока.
func TestThrottledProcsCachesError(t *testing.T) {
	calls := 0
	boom := errors.New("boom")
	real := func() (map[string]int, error) {
		calls++
		return nil, boom
	}
	now := time.Unix(1000, 0)
	throttled := throttledProcs(real, func() time.Time { return now }, 60*time.Second)

	if _, err := throttled(); err != boom {
		t.Fatalf("первый вызов err = %v, хочу %v", err, boom)
	}
	if _, err := throttled(); err != boom {
		t.Fatalf("второй вызов (кэш) err = %v, хочу тот же %v", err, boom)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, хочу 1 (второй вызов отдан из кэша)", calls)
	}
}

// TestThrottledProcsKeepsSampleNonEmpty: тик без реального опроса процессов
// (внутри procsProbeInterval) всё равно получает непустой Sample.Procs из
// кэша — Collect не считает такой тик "пустым" (см. sampleEmpty в run.go).
func TestThrottledProcsKeepsSampleNonEmpty(t *testing.T) {
	now := time.Unix(1000, 0)
	probes := fakeProbes()
	probes.Procs = throttledProcs(probes.Procs, func() time.Time { return now }, 60*time.Second)
	c := NewCollector(probes)

	s1, err := c.Collect(now)
	if err != nil {
		t.Fatalf("Collect (первый тик): %v", err)
	}
	if s1.Procs == nil {
		t.Fatal("s1.Procs == nil на первом тике")
	}

	now = now.Add(10 * time.Second) // внутри interval — реального опроса не будет
	s2, err := c.Collect(now)
	if err != nil {
		t.Fatalf("Collect (тик без реального опроса процессов): %v", err)
	}
	if s2.Procs == nil {
		t.Fatal("s2.Procs == nil на тике без реального опроса — должен остаться кэш")
	}
	if sampleEmpty(s2) {
		t.Fatal("sampleEmpty(s2) = true — тик без реального опроса процессов не должен выглядеть пустым")
	}
}
