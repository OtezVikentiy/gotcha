package metric

import (
	"bytes"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// captureWarnLog подменяет slog по умолчанию буфером на время теста.
func captureWarnLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// resetClockSkew обнуляет процесс-локальный учёт между тестами.
func resetClockSkew(t *testing.T) {
	t.Helper()
	clockSkew = clockSkewStats{}
	t.Cleanup(func() { clockSkew = clockSkewStats{} })
}

func hostGaugeMetrics(host string, ts uint64) []*metricspb.ResourceMetrics {
	return []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "host.name", Value: strVal(host)},
		}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: "g", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
				TimeUnixNano: ts,
				Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 1},
			}}}},
		}}}},
	}}
}

// TestPointTimeClampsFutureToFallback — K3-1: время из будущего не дропается
// и не принимается как есть, а приводится к моменту приёма; ahead — на
// сколько точка опережала. Прошлое внутри окна проходит без изменений,
// старше окна — дропается по-прежнему.
func TestPointTimeClampsFutureToFallback(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		ts        time.Time
		wantTS    time.Time
		wantAhead time.Duration
		wantOK    bool
	}{
		{"past in window", now.Add(-time.Hour), now.Add(-time.Hour), 0, true},
		{"exactly now", now, now, 0, true},
		{"jitter ahead", now.Add(300 * time.Millisecond), now, 300 * time.Millisecond, true},
		{"minutes ahead", now.Add(7 * time.Minute), now, 7 * time.Minute, true},
		{"days ahead (was dropped before)", now.Add(48 * time.Hour), now, 48 * time.Hour, true},
		{"older than retention", now.Add(-maxPointAge - time.Second), time.Time{}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts, ahead, ok := pointTime(uint64(c.ts.UnixNano()), now)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ts.Equal(c.wantTS) {
				t.Fatalf("ts = %v, want %v", ts, c.wantTS)
			}
			if ahead != c.wantAhead {
				t.Fatalf("ahead = %v, want %v", ahead, c.wantAhead)
			}
		})
	}
}

// TestMapOTLPClampsFutureAndCounts — сквозь MapOTLP: точка из будущего
// попадает в вывод с TS == момент приёма (раньше дропалась, если опережала
// больше суток, и принималась как есть, если меньше), и каждая такая точка
// увеличивает счётчик self-метрики. Точки без опережения счётчик не трогают.
func TestMapOTLPClampsFutureAndCounts(t *testing.T) {
	resetClockSkew(t)
	captureWarnLog(t)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	nearFuture := uint64(now.Add(30 * time.Second).UnixNano())
	farFuture := uint64(now.Add(48 * time.Hour).UnixNano())
	past := uint64(now.Add(-time.Minute).UnixNano())

	pts := MapOTLP(gaugeResourceMetrics(t, map[uint64]float64{nearFuture: 1, farFuture: 2, past: 3}), now)
	if len(pts) != 3 {
		t.Fatalf("points = %d, want 3 (future points are clamped, not dropped)", len(pts))
	}
	for _, p := range pts {
		switch p.Value {
		case 1, 2:
			if !p.TS.Equal(now) {
				t.Errorf("value %v: TS = %v, want clamped to receive time %v", p.Value, p.TS, now)
			}
		case 3:
			if !p.TS.Equal(now.Add(-time.Minute)) {
				t.Errorf("past point TS = %v, want untouched", p.TS)
			}
		}
	}
	if got := ClockSkewPoints(); got != 2 {
		t.Fatalf("ClockSkewPoints = %d, want 2", got)
	}

	MapOTLP(gaugeResourceMetrics(t, map[uint64]float64{past: 4, 0: 5}), now)
	if got := ClockSkewPoints(); got != 2 {
		t.Fatalf("ClockSkewPoints after in-window batch = %d, want still 2", got)
	}
}

// TestClockSkewLogThresholdAndRateLimit — лог пишется только при опережении
// не меньше минуты, не чаще раза в минуту на процесс, и одна запись — отчёт
// за период: сколько точек клэмпнуто с прошлой записи и максимальное
// опережение, плюс имя хоста, раз оно под рукой у парсера.
func TestClockSkewLogThresholdAndRateLimit(t *testing.T) {
	resetClockSkew(t)
	buf := captureWarnLog(t)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	// Джиттер: клэмпится и считается, но в лог не идёт.
	MapOTLP(hostGaugeMetrics("web-1", uint64(now.Add(5*time.Second).UnixNano())), now)
	if buf.Len() != 0 {
		t.Fatalf("jitter below threshold must not log, got: %s", buf.String())
	}
	if got := ClockSkewPoints(); got != 1 {
		t.Fatalf("ClockSkewPoints = %d, want 1 (jitter is still counted)", got)
	}

	// Опережение выше порога: первая запись — с накопленным «сколько» (2:
	// джиттер + эта точка), максимальным опережением и хостом.
	MapOTLP(hostGaugeMetrics("web-1", uint64(now.Add(3*time.Minute).UnixNano())), now)
	first := buf.String()
	if !strings.Contains(first, "clamped to the receive time") {
		t.Fatalf("expected clamp warning, got: %q", first)
	}
	for _, want := range []string{"points=2", "max_ahead=3m0s", "host=web-1"} {
		if !strings.Contains(first, want) {
			t.Errorf("log lacks %q: %s", want, first)
		}
	}

	// Та же минута: вторая точка выше порога — молча (ограничение частоты),
	// но накапливается.
	buf.Reset()
	MapOTLP(hostGaugeMetrics("web-2", uint64(now.Add(20*time.Second+10*time.Minute).UnixNano())), now.Add(20*time.Second))
	if buf.Len() != 0 {
		t.Fatalf("second warning within a minute must be suppressed, got: %s", buf.String())
	}

	// Минута прошла: новая запись — про накопленное с прошлой (1 точка, 10m).
	MapOTLP(hostGaugeMetrics("web-2", uint64(now.Add(2*time.Minute).UnixNano())), now.Add(clockSkewLogInterval))
	second := buf.String()
	if strings.Count(second, "clamped to the receive time") != 1 {
		t.Fatalf("expected exactly one warning after the interval, got: %q", second)
	}
	for _, want := range []string{"points=2", "max_ahead=10m0s", "host=web-2"} {
		if !strings.Contains(second, want) {
			t.Errorf("second log lacks %q: %s", want, second)
		}
	}
	if got := ClockSkewPoints(); got != 4 {
		t.Fatalf("ClockSkewPoints = %d, want 4", got)
	}
}

// TestClockSkewNoteConcurrentSingleLog — параллельные приёмники в одну минуту:
// право на запись в лог берётся CAS'ом по lastLog, проигравшие CAS выходят
// молча, и запись ровно одна; при этом ни один вклад в total/pending/maxAhead
// не теряется — сумма «сколько» по всем записям за все раунды равна числу
// вызовов, а максимум — максимальному опережению. Раундов много, а старт —
// по атомарному флагу, на котором горутины крутятся вхолостую (а не по
// закрытию канала, из которого рантайм будит их по одной): только так окно
// между Load и CompareAndSwap у нескольких потоков реально пересекается, и
// ветка проигравшего CAS исполняется, а не только могла бы.
func TestClockSkewNoteConcurrentSingleLog(t *testing.T) {
	resetClockSkew(t)
	buf := captureWarnLog(t)
	goroutines := 4 * runtime.GOMAXPROCS(0)
	if goroutines < 16 {
		goroutines = 16
	}
	const rounds = 1000
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	var seen uint64 // сумма points= по всем записям в лог
	for round := 0; round < rounds; round++ {
		clockSkew = clockSkewStats{}
		buf.Reset()
		var start atomic.Bool
		var ready atomic.Int32
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				ready.Add(1)
				for !start.Load() {
					runtime.Gosched()
				}
				clockSkew.note(clockSkewLogThreshold+time.Duration(g)*time.Second, "web-1", now)
			}(g)
		}
		for int(ready.Load()) < goroutines {
			runtime.Gosched()
		}
		start.Store(true)
		wg.Wait()

		logs := buf.String()
		if n := strings.Count(logs, "clamped to the receive time"); n != 1 {
			t.Fatalf("round %d: %d warnings from %d concurrent callers, want exactly 1:\n%s", round, n, goroutines, logs)
		}
		if got := clockSkew.total.Load(); got != uint64(goroutines) {
			t.Fatalf("round %d: total = %d, want %d (a contribution was lost)", round, got, goroutines)
		}
		// Записанное «сколько» плюс то, что осталось на следующий период, —
		// ровно число вызовов: вклад ни потерян, ни посчитан дважды.
		var logged uint64
		for _, f := range strings.Fields(logs) {
			if v, ok := strings.CutPrefix(f, "points="); ok {
				n, err := strconv.ParseUint(v, 10, 64)
				if err != nil {
					t.Fatalf("round %d: bad points= field %q: %v", round, v, err)
				}
				logged = n
			}
		}
		if logged+clockSkew.pending.Load() != uint64(goroutines) {
			t.Fatalf("round %d: logged %d + pending %d != %d calls", round, logged, clockSkew.pending.Load(), goroutines)
		}
		seen += logged
		// Самый большой вклад либо ушёл в лог, либо ещё ждёт следующей
		// записи — потеряться он не может.
		wantMax := clockSkewLogThreshold + time.Duration(goroutines-1)*time.Second
		if !strings.Contains(logs, "max_ahead="+wantMax.String()) && time.Duration(clockSkew.maxAhead.Load()) != wantMax {
			t.Fatalf("round %d: largest contribution %v neither logged nor pending (pending max %v):\n%s",
				round, wantMax, time.Duration(clockSkew.maxAhead.Load()), logs)
		}
	}
	if seen == 0 {
		t.Fatalf("no points= field parsed across %d rounds", rounds)
	}
}
