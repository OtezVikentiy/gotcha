package trace

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

var detectBase = time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)

// mkSpan — спан с заданным родителем, op, описанием и длительностью в мс.
func mkSpan(id, parent, op, desc string, ms int) Span {
	return Span{
		SpanID:       id,
		ParentSpanID: parent,
		Op:           op,
		Description:  desc,
		Start:        detectBase,
		End:          detectBase.Add(time.Duration(ms) * time.Millisecond),
	}
}

// mkSpanAt — спан, начинающийся через offMs от начала транзакции: нужен там, где
// важно взаимное расположение спанов во времени (последовательные вызовы против
// параллельных), а не только их длительность.
func mkSpanAt(id, parent, op, desc string, offMs, ms int) Span {
	start := detectBase.Add(time.Duration(offMs) * time.Millisecond)
	return Span{
		SpanID:       id,
		ParentSpanID: parent,
		Op:           op,
		Description:  desc,
		Start:        start,
		End:          start.Add(time.Duration(ms) * time.Millisecond),
	}
}

// mkTx — транзакция длительностью ms с указанными спанами; корень — "root".
func mkTx(ms int, spans ...Span) Transaction {
	return Transaction{
		TraceID: "trace-1",
		SpanID:  "root",
		Name:    "GET /orders",
		Op:      "http.server",
		Start:   detectBase,
		End:     detectBase.Add(time.Duration(ms) * time.Millisecond),
		Spans:   spans,
	}
}

// repeatSpans — n спанов с одним родителем и одинаковым описанием.
func repeatSpans(n int, parent, op, desc string, ms int) []Span {
	out := make([]Span, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, mkSpan(fmt.Sprintf("s%d", i), parent, op, desc, ms))
	}
	return out
}

// kindsOf — только виды находок, в порядке возврата.
func kindsOf(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Kind)
	}
	return out
}

func findingOf(t *testing.T, fs []Finding, kind string) Finding {
	t.Helper()
	for _, f := range fs {
		if f.Kind == kind {
			return f
		}
	}
	t.Fatalf("находка вида %q не найдена, есть: %v", kind, kindsOf(fs))
	return Finding{}
}

func TestDetect(t *testing.T) {
	const sel = "SELECT * FROM users WHERE id = 42"

	differentQueries := []Span{
		mkSpan("a", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 5),
		mkSpan("b", "p1", "db.sql.query", "SELECT * FROM orders WHERE id = 1", 5),
		mkSpan("c", "p1", "db.sql.query", "UPDATE users SET name = 'x' WHERE id = 1", 5),
		mkSpan("d", "p1", "db.sql.query", "DELETE FROM carts WHERE id = 1", 5),
		mkSpan("e", "p1", "db.sql.query", "INSERT INTO logs (msg) VALUES ('x')", 5),
	}

	literalVariants := []Span{
		mkSpan("a", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 5),
		mkSpan("b", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 2", 5),
		mkSpan("c", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 3", 5),
		mkSpan("d", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 4", 5),
		mkSpan("e", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 5", 5),
	}

	differentParents := []Span{
		mkSpan("a", "p1", "db.sql.query", sel, 5),
		mkSpan("b", "p2", "db.sql.query", sel, 5),
		mkSpan("c", "p3", "db.sql.query", sel, 5),
		mkSpan("d", "p4", "db.sql.query", sel, 5),
		mkSpan("e", "p5", "db.sql.query", sel, 5),
	}

	// Вызовы идут ВСТЫК: http_flood — про кандидата на распараллеливание, и
	// параллельный веер он (осознанно) не показывает, см. detectHTTPFlood.
	httpSpans := func(n int) []Span {
		out := make([]Span, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, mkSpanAt(fmt.Sprintf("h%d", i), "root", "http.client",
				fmt.Sprintf("GET https://api.example.com/users/%d", i), i*100, 100))
		}
		return out
	}

	cases := []struct {
		name string
		tx   Transaction
		cfg  DetectorConfig
		want []string // ожидаемые виды находок, в порядке возврата
	}{
		{
			name: "5 одинаковых db-спанов под одним родителем при пороге 5 -> N+1",
			tx:   mkTx(1000, repeatSpans(5, "p1", "db.sql.query", sel, 5)...),
			cfg:  DefaultDetectorConfig(),
			want: []string{KindNPlusOne},
		},
		{
			name: "4 таких же -> НЕТ",
			tx:   mkTx(1000, repeatSpans(4, "p1", "db.sql.query", sel, 5)...),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "5 одинаковых под РАЗНЫМИ родителями -> НЕТ",
			tx:   mkTx(1000, differentParents...),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "5 РАЗНЫХ запросов под одним родителем -> НЕТ",
			tx:   mkTx(1000, differentQueries...),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "5 одинаковых по структуре с разными литералами -> N+1 (нормализация)",
			tx:   mkTx(1000, literalVariants...),
			cfg:  DefaultDetectorConfig(),
			want: []string{KindNPlusOne},
		},
		{
			name: "op=db (OTLP) тоже считается",
			tx:   mkTx(1000, repeatSpans(5, "p1", "db", sel, 5)...),
			cfg:  DefaultDetectorConfig(),
			want: []string{KindNPlusOne},
		},
		{
			name: "op=db.redis тоже считается",
			tx:   mkTx(1000, repeatSpans(6, "p1", "db.redis", "GET user:1", 5)...),
			cfg:  DefaultDetectorConfig(),
			want: []string{KindNPlusOne},
		},
		{
			name: "не-db спаны не группируются в N+1",
			tx:   mkTx(1000, repeatSpans(9, "p1", "template.render", "user_row.html", 1)...),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "порог из конфига: 3 спана при NPlusOneMin=3 -> N+1",
			tx:   mkTx(1000, repeatSpans(3, "p1", "db.sql.query", sel, 10)...),
			cfg:  DetectorConfig{NPlusOneMin: 3, SlowDBMs: 500, HTTPFloodMin: 10},
			want: []string{KindNPlusOne},
		},
		{
			name: "db-спан 600мс при пороге 500 -> slow",
			tx:   mkTx(5000, mkSpan("a", "root", "db.sql.query", sel, 600)),
			cfg:  DefaultDetectorConfig(),
			want: []string{KindSlowDBQuery},
		},
		{
			name: "db-спан 500мс при пороге 500 -> НЕТ (строго больше)",
			tx:   mkTx(5000, mkSpan("a", "root", "db.sql.query", sel, 500)),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "db-спан 150мс при транзакции 200мс (75% > 30%, выше пола) -> slow",
			tx:   mkTx(200, mkSpan("a", "root", "db.sql.query", sel, 150)),
			cfg:  DefaultDetectorConfig(),
			want: []string{KindSlowDBQuery},
		},
		{
			name: "db-спан 8мс при транзакции 12мс (67% > 30%, но ниже пола) -> НЕТ",
			tx:   mkTx(12, mkSpan("a", "root", "db.sql.query", sel, 8)),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "db-спан 20мс при транзакции 200мс (10% < 30%) -> НЕТ",
			tx:   mkTx(200, mkSpan("a", "root", "db.sql.query", sel, 20)),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "медленный http-спан не slow_db_query",
			tx:   mkTx(1000, mkSpan("a", "root", "http.client", "GET https://api.example.com/users", 900)),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "10 http.client спанов при пороге 10 -> flood",
			tx:   mkTx(5000, httpSpans(10)...),
			cfg:  DefaultDetectorConfig(),
			want: []string{KindHTTPFlood},
		},
		{
			name: "9 http.client спанов -> НЕТ",
			tx:   mkTx(5000, httpSpans(9)...),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "10 http.server спанов не flood",
			tx:   mkTx(5000, repeatSpans(10, "root", "http.server", "GET /x", 1)...),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "транзакция без спанов -> пусто",
			tx:   mkTx(1000),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "нулевая длительность транзакции: без паники, без деления на ноль",
			tx:   mkTx(0, mkSpan("a", "root", "db.sql.query", sel, 0)),
			cfg:  DefaultDetectorConfig(),
			want: nil,
		},
		{
			name: "нулевая длительность транзакции и долгий db-спан -> slow по абсолютному порогу",
			tx:   mkTx(0, mkSpan("a", "root", "db.sql.query", sel, 600)),
			cfg:  DefaultDetectorConfig(),
			want: []string{KindSlowDBQuery},
		},
		{
			name: "порядок находок стабилен: n+1, затем slow, затем flood",
			tx: mkTx(1000, append(append(
				repeatSpans(5, "p1", "db.sql.query", sel, 600),
				httpSpans(10)...), mkSpan("z", "root", "db.sql.query", "SELECT 1", 800))...),
			cfg:  DefaultDetectorConfig(),
			want: []string{KindNPlusOne, KindSlowDBQuery, KindSlowDBQuery, KindHTTPFlood},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect(tc.tx, tc.cfg)
			if diff := kindsOf(got); !reflect.DeepEqual(diff, tc.want) && !(len(diff) == 0 && len(tc.want) == 0) {
				t.Fatalf("Detect() виды = %v, want %v (находки: %+v)", diff, tc.want, got)
			}
		})
	}
}

func TestDetectNPlusOneFinding(t *testing.T) {
	spans := []Span{
		mkSpan("p1", "root", "db.sql.query", "SELECT * FROM orders", 5),
		mkSpan("a", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 10),
		mkSpan("b", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 2", 10),
		mkSpan("c", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 3", 10),
		mkSpan("d", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 4", 10),
		mkSpan("e", "p1", "db.sql.query", "SELECT * FROM users WHERE id = 5", 20),
	}
	got := Detect(mkTx(1000, spans...), DefaultDetectorConfig())
	f := findingOf(t, got, KindNPlusOne)

	if want := "SELECT * FROM users WHERE id = ?"; f.Description != want {
		t.Errorf("Description = %q, want %q", f.Description, want)
	}
	if want := "N+1 запросов: SELECT * FROM users WHERE id = ?"; f.Title != want {
		t.Errorf("Title = %q, want %q", f.Title, want)
	}
	if f.Culprit != "GET /orders" {
		t.Errorf("Culprit = %q, want %q", f.Culprit, "GET /orders")
	}
	if f.Fingerprint == "" {
		t.Error("Fingerprint пустой")
	}
	if got, want := f.Evidence["count"], 5; got != want {
		t.Errorf("evidence count = %v, want %v", got, want)
	}
	if got, want := f.Evidence["total_us"], int64(60_000); got != want {
		t.Errorf("evidence total_us = %v, want %v", got, want)
	}
	if got, want := f.Evidence["parent_op"], "db.sql.query"; got != want {
		t.Errorf("evidence parent_op = %v, want %v", got, want)
	}
	ids, ok := f.Evidence["span_ids"].([]string)
	if !ok {
		t.Fatalf("evidence span_ids имеет тип %T, want []string", f.Evidence["span_ids"])
	}
	if want := []string{"a", "b", "c", "d", "e"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("evidence span_ids = %v, want %v", ids, want)
	}
}

// Родитель N+1 — корневой спан транзакции: parent_op берётся у транзакции.
func TestDetectNPlusOneParentIsRoot(t *testing.T) {
	got := Detect(mkTx(1000, repeatSpans(5, "root", "db.sql.query", "SELECT 1", 5)...), DefaultDetectorConfig())
	f := findingOf(t, got, KindNPlusOne)
	if got, want := f.Evidence["parent_op"], "http.server"; got != want {
		t.Errorf("evidence parent_op = %v, want %v", got, want)
	}
}

func TestDetectSpanIDsCappedAtTen(t *testing.T) {
	got := Detect(mkTx(1000, repeatSpans(25, "p1", "db.sql.query", "SELECT 1", 1)...), DefaultDetectorConfig())
	f := findingOf(t, got, KindNPlusOne)
	if got, want := f.Evidence["count"], 25; got != want {
		t.Errorf("evidence count = %v, want %v", got, want)
	}
	ids := f.Evidence["span_ids"].([]string)
	if len(ids) != maxEvidenceSpanIDs {
		t.Errorf("len(span_ids) = %d, want %d", len(ids), maxEvidenceSpanIDs)
	}
}

// Один и тот же медленный запрос дважды — это одна проблема с count=2.
func TestDetectSlowDBQueryDeduplicated(t *testing.T) {
	spans := []Span{
		mkSpan("a", "root", "db.sql.query", "SELECT * FROM reports WHERE year = 2024", 600),
		mkSpan("b", "root", "db.sql.query", "SELECT * FROM reports WHERE year = 2025", 700),
	}
	got := Detect(mkTx(5000, spans...), DefaultDetectorConfig())
	if len(got) != 1 {
		t.Fatalf("находок %d, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Kind != KindSlowDBQuery {
		t.Fatalf("Kind = %q", f.Kind)
	}
	if want := "SELECT * FROM reports WHERE year = ?"; f.Description != want {
		t.Errorf("Description = %q, want %q", f.Description, want)
	}
	if want := "Медленный запрос: SELECT * FROM reports WHERE year = ?"; f.Title != want {
		t.Errorf("Title = %q, want %q", f.Title, want)
	}
	if got, want := f.Evidence["count"], 2; got != want {
		t.Errorf("evidence count = %v, want %v", got, want)
	}
	if got, want := f.Evidence["total_us"], int64(1_300_000); got != want {
		t.Errorf("evidence total_us = %v, want %v", got, want)
	}
	if got, want := f.Evidence["max_us"], int64(700_000); got != want {
		t.Errorf("evidence max_us = %v, want %v", got, want)
	}
}

func TestDetectHTTPFloodEvidence(t *testing.T) {
	// 10 вызовов по 90мс друг за другом: суммарно 900мс и по стене 900мс.
	seq := make([]Span, 0, 10)
	for i := 0; i < 10; i++ {
		seq = append(seq, mkSpanAt(fmt.Sprintf("h%d", i), "root", "http.client",
			fmt.Sprintf("GET https://api.example.com/users/%d?page=2", i), i*90, 90))
	}
	f := findingOf(t, Detect(mkTx(1000, seq...), DefaultDetectorConfig()), KindHTTPFlood)
	if got, want := f.Evidence["count"], 10; got != want {
		t.Errorf("evidence count = %v, want %v", got, want)
	}
	if got, want := f.Evidence["total_us"], int64(900_000); got != want {
		t.Errorf("evidence total_us = %v, want %v", got, want)
	}
	// 10 вызовов по стене = сумме: полностью последовательны, доля 100%.
	if got, want := f.Evidence["sequential_pct"], 100; got != want {
		t.Errorf("evidence sequential_pct = %v, want %v", got, want)
	}
	if _, ok := f.Evidence["sequential"]; ok {
		t.Error("evidence sequential: флаг после гейта всегда true, заменён на sequential_pct")
	}
	if got, want := f.Evidence["max_concurrency"], 1; got != want {
		t.Errorf("evidence max_concurrency = %v, want %v", got, want)
	}
	if got, want := f.Culprit, "GET /orders"; got != want {
		t.Errorf("Culprit = %q, want %q", got, want)
	}
	if len(f.Evidence["span_ids"].([]string)) != maxEvidenceSpanIDs {
		t.Errorf("span_ids не закаппены: %v", f.Evidence["span_ids"])
	}

	// urls: нормализованные адреса (без query и без id) — чтобы со страницы
	// проблемы было видно, КУДА ходили; фингерпринт их не учитывает.
	urls, ok := f.Evidence["urls"].([]string)
	if !ok {
		t.Fatalf("evidence urls имеет тип %T, want []string", f.Evidence["urls"])
	}
	if len(urls) > maxEvidenceURLs {
		t.Fatalf("urls не закаппены: %d штук", len(urls))
	}
	if len(urls) == 0 || urls[0] != "GET https://api.example.com/users/{id}" {
		t.Errorf("urls = %v", urls)
	}

	// Нулевая длительность транзакции: паники нет.
	zero := findingOf(t, Detect(mkTx(0, seq...), DefaultDetectorConfig()), KindHTTPFlood)
	if zero.Evidence["count"] != 10 {
		t.Errorf("нулевая транзакция: count = %v", zero.Evidence["count"])
	}
}

// Здоровый эндпойнт (12мс запроса, из них 8мс — единственный SELECT) не проблема:
// у правила «доля от транзакции» есть абсолютный пол SlowDBMs/5.
func TestDetectSlowDBQueryShareHasFloor(t *testing.T) {
	const sel = "SELECT * FROM users WHERE id = 42"
	cfg := DefaultDetectorConfig()

	healthy := Detect(mkTx(12, mkSpan("a", "root", "db.sql.query", sel, 8)), cfg)
	if len(healthy) != 0 {
		t.Fatalf("здоровый 12мс эндпойнт с 8мс запросом дал находки: %+v", healthy)
	}

	// Абсолютный порог: 600мс > 500мс.
	if got := kindsOf(Detect(mkTx(5000, mkSpan("a", "root", "db.sql.query", sel, 600)), cfg)); len(got) != 1 {
		t.Errorf("600мс запрос: находки = %v, want [slow_db_query]", got)
	}
	// Доля: 150мс (> пола 100мс) в транзакции 200мс.
	if got := kindsOf(Detect(mkTx(200, mkSpan("a", "root", "db.sql.query", sel, 150)), cfg)); len(got) != 1 {
		t.Errorf("150мс запрос в 200мс транзакции: находки = %v, want [slow_db_query]", got)
	}
	// Пол считается от конфига: при SlowDBMs=50 пол 10мс, и 8мс запрос в 12мс
	// транзакции уже проблема.
	tight := DetectorConfig{NPlusOneMin: 5, SlowDBMs: 50, HTTPFloodMin: 10}
	if got := kindsOf(Detect(mkTx(40, mkSpan("a", "root", "db.sql.query", sel, 20)), tight)); len(got) != 1 {
		t.Errorf("20мс запрос при SlowDBMs=50: находки = %v, want [slow_db_query]", got)
	}
}

// Redis N+1: ленивая подгрузка кеша ходит за РАЗНЫМИ ключами — именно это и
// должно схлопываться в одну находку.
func TestDetectRedisNPlusOneDistinctKeys(t *testing.T) {
	spans := make([]Span, 0, 8)
	for i := 0; i < 8; i++ {
		spans = append(spans, mkSpan(fmt.Sprintf("r%d", i), "p1", "db.redis",
			fmt.Sprintf("GET user:%d", i), 5))
	}
	f := findingOf(t, Detect(mkTx(100, spans...), DefaultDetectorConfig()), KindNPlusOne)
	if want := "GET user:?"; f.Description != want {
		t.Errorf("Description = %q, want %q", f.Description, want)
	}
	if got, want := f.Evidence["count"], 8; got != want {
		t.Errorf("evidence count = %v, want %v", got, want)
	}
}

// Субмиллисекундные спаны (Redis) не должны схлопываться в нулевую длительность:
// evidence считается в микросекундах.
func TestDetectEvidenceInMicroseconds(t *testing.T) {
	spans := make([]Span, 0, 6)
	for i := 0; i < 6; i++ {
		s := mkSpan(fmt.Sprintf("r%d", i), "p1", "db.redis", fmt.Sprintf("GET user:%d", i), 0)
		s.End = s.Start.Add(300 * time.Microsecond)
		spans = append(spans, s)
	}
	// Пол по суммарному времени опущен конфигом: 6 x 300мкс = 1.8мс, на дефолтных
	// 20мс такой цикл (правильно) не находка — здесь проверяются ЕДИНИЦЫ evidence.
	cfg := DetectorConfig{NPlusOneMin: 5, NPlusOneMinTotalMs: 1, SlowDBMs: 500, HTTPFloodMin: 10}
	f := findingOf(t, Detect(mkTx(100, spans...), cfg), KindNPlusOne)
	if got, want := f.Evidence["total_us"], int64(1800); got != want {
		t.Errorf("evidence total_us = %v, want %v", got, want)
	}
}

// Одна транзакция — не больше maxFindings находок: враждебный (или просто
// сломанный) энвелоп с сотней разных медленных запросов не должен превращаться в
// сотню строк perf_issues и сотню задач в outbox.
func TestDetectCapsFindingsPerTransaction(t *testing.T) {
	spans := make([]Span, 0, 100)
	for i := 0; i < 100; i++ {
		spans = append(spans, mkSpan(fmt.Sprintf("s%d", i), "root", "db.sql.query",
			fmt.Sprintf("SELECT * FROM t%d WHERE id = 1", i), 600+i))
	}
	got := Detect(mkTx(100000, spans...), DefaultDetectorConfig())
	if len(got) != maxFindingsPerTransaction {
		t.Fatalf("находок %d, want %d", len(got), maxFindingsPerTransaction)
	}
	// Оставлены самые тяжёлые: 600+99 = 699мс самый долгий.
	if got[0].Evidence["total_us"] != int64(699_000) {
		t.Errorf("первая находка не самая тяжёлая: %+v", got[0].Evidence)
	}
	// Кап детерминирован.
	if second := Detect(mkTx(100000, spans...), DefaultDetectorConfig()); !reflect.DeepEqual(second, got) {
		t.Error("кап не детерминирован")
	}
}

// Один и тот же fingerprint (один запрос, N+1 под ДВУМЯ родителями) — одна
// находка: иначе Record зовётся дважды за транзакцию, count растёт на 2, а
// evidence второй группы затирает первую.
func TestDetectMergesFindingsWithSameFingerprint(t *testing.T) {
	const sel = "SELECT * FROM users WHERE id = 1"
	spans := append(repeatSpans(5, "p1", "db.sql.query", sel, 10),
		mkSpan("x0", "p2", "db.sql.query", sel, 10),
		mkSpan("x1", "p2", "db.sql.query", sel, 10),
		mkSpan("x2", "p2", "db.sql.query", sel, 10),
		mkSpan("x3", "p2", "db.sql.query", sel, 10),
		mkSpan("x4", "p2", "db.sql.query", sel, 10))

	got := Detect(mkTx(1000, spans...), DefaultDetectorConfig())
	if len(got) != 1 {
		t.Fatalf("находок %d, want 1: %+v", len(got), got)
	}
	if c := got[0].Evidence["count"]; c != 10 {
		t.Errorf("evidence count = %v, want 10 (суммируются обе группы)", c)
	}
	if us := got[0].Evidence["total_us"]; us != int64(100_000) {
		t.Errorf("evidence total_us = %v, want 100000", us)
	}
	ids := got[0].Evidence["span_ids"].([]string)
	if len(ids) != maxEvidenceSpanIDs {
		t.Errorf("span_ids = %v, want %d штук (объединение обеих групп)", ids, maxEvidenceSpanIDs)
	}
}

// Фингерпринт: одинаковый для одной и той же проблемы (в т.ч. с другими
// литералами и в другой транзакции того же эндпойнта), разный — для разных.
func TestFindingFingerprintStable(t *testing.T) {
	cfg := DefaultDetectorConfig()

	a := Detect(mkTx(1000, repeatSpans(5, "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 5)...), cfg)
	b := Detect(mkTx(2000, repeatSpans(7, "p9", "db.sql.query", "SELECT * FROM users WHERE id = 777", 3)...), cfg)
	if a[0].Fingerprint != b[0].Fingerprint {
		t.Errorf("фингерпринты одной проблемы разошлись: %q != %q", a[0].Fingerprint, b[0].Fingerprint)
	}

	// Другая транзакция (culprit) — другая проблема.
	txOther := mkTx(1000, repeatSpans(5, "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 5)...)
	txOther.Name = "GET /profile"
	c := Detect(txOther, cfg)
	if c[0].Fingerprint == a[0].Fingerprint {
		t.Error("разные транзакции дали одинаковый фингерпринт")
	}

	// Другой запрос — другая проблема.
	d := Detect(mkTx(1000, repeatSpans(5, "p1", "db.sql.query", "SELECT * FROM orders WHERE id = 1", 5)...), cfg)
	if d[0].Fingerprint == a[0].Fingerprint {
		t.Error("разные запросы дали одинаковый фингерпринт")
	}

	// Другой вид проблемы при том же описании — другая проблема.
	slow := Detect(mkTx(5000, mkSpan("x", "root", "db.sql.query", "SELECT * FROM users WHERE id = 1", 600)), cfg)
	if slow[0].Fingerprint == a[0].Fingerprint {
		t.Error("разные виды проблем дали одинаковый фингерпринт")
	}
}

// Detect — чистая функция: тот же вход даёт тот же выход.
func TestDetectDeterministic(t *testing.T) {
	tx := mkTx(1000, append(
		repeatSpans(6, "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 600),
		repeatSpans(12, "root", "http.client", "GET https://api.example.com/x", 10)...)...)

	first := Detect(tx, DefaultDetectorConfig())
	for i := 0; i < 20; i++ {
		if got := Detect(tx, DefaultDetectorConfig()); !reflect.DeepEqual(got, first) {
			t.Fatalf("прогон %d дал другой результат:\n%+v\nvs\n%+v", i, got, first)
		}
	}
}

func TestDefaultDetectorConfig(t *testing.T) {
	cfg := DefaultDetectorConfig()
	// NPlusOneMinTotalMs проверяется отдельно: его регресс к нулю сделал бы N+1
	// «срабатывающим всегда» (пол по времени исчез бы), и ни один другой тест
	// этого бы не поймал.
	if cfg.NPlusOneMin != 5 || cfg.NPlusOneMinTotalMs != 20 || cfg.SlowDBMs != 500 || cfg.HTTPFloodMin != 10 {
		t.Errorf("DefaultDetectorConfig() = %+v", cfg)
	}
}

func TestConfigFromJSON(t *testing.T) {
	def := DefaultDetectorConfig()

	cases := []struct {
		name    string
		raw     string
		want    DetectorConfig
		wantErr bool
	}{
		{name: "nil -> дефолты", raw: "", want: def},
		{name: "пустой объект -> дефолты", raw: `{}`, want: def},
		{name: "null -> дефолты", raw: `null`, want: def},
		{
			name: "все поля",
			raw:  `{"n_plus_one_min":3,"n_plus_one_min_total_ms":50,"slow_db_ms":100,"http_flood_min":20}`,
			want: DetectorConfig{NPlusOneMin: 3, NPlusOneMinTotalMs: 50, SlowDBMs: 100, HTTPFloodMin: 20},
		},
		{
			name: "частичный конфиг: отсутствующие поля — дефолты",
			raw:  `{"slow_db_ms":250}`,
			want: DetectorConfig{NPlusOneMin: def.NPlusOneMin, NPlusOneMinTotalMs: def.NPlusOneMinTotalMs,
				SlowDBMs: 250, HTTPFloodMin: def.HTTPFloodMin},
		},
		{
			name: "нулевые и отрицательные значения — дефолты",
			raw:  `{"n_plus_one_min":0,"n_plus_one_min_total_ms":-5,"slow_db_ms":-1,"http_flood_min":0}`,
			want: def,
		},
		{name: "мусор -> ошибка", raw: `{`, want: def, wantErr: true},
		{name: "не объект -> ошибка", raw: `[1,2]`, want: def, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if tc.raw != "" {
				raw = []byte(tc.raw)
			}
			got, err := ConfigFromJSON(raw)
			if tc.wantErr && err == nil {
				t.Fatal("ошибка ожидалась, получен nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if got != tc.want {
				t.Errorf("ConfigFromJSON(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// Нулевой конфиг (никто не вызвал ConfigFromJSON) не должен превращать
// детекторы в детекторы всего подряд: пороги подставляются дефолтные.
func TestDetectZeroConfigUsesDefaults(t *testing.T) {
	tx := mkTx(1000, repeatSpans(4, "p1", "db.sql.query", "SELECT 1", 1)...)
	if got := Detect(tx, DetectorConfig{}); len(got) != 0 {
		t.Fatalf("нулевой конфиг сработал на 4 спанах: %+v", got)
	}
}

// --- регрессии ревью этапа 3 ---

// mkSpanUS — спан длительностью в МИКРОсекундах: Redis-спаны бывают
// субмиллисекундными, а именно на них проверяется пол по времени у N+1.
func mkSpanUS(id, parent, op, desc string, us int) Span {
	return Span{
		SpanID:       id,
		ParentSpanID: parent,
		Op:           op,
		Description:  desc,
		Start:        detectBase,
		End:          detectBase.Add(time.Duration(us) * time.Microsecond),
	}
}

// Эндпойнт без шаблонизации маршрута (`GET /users/42`, `GET /users/43`, ...) с
// одним и тем же N+1 — это ОДНА проблема, а не одна на каждый запрос: в
// фингерпринт идёт НОРМАЛИЗОВАННОЕ имя транзакции. Иначе 100 запросов дали бы
// 100 строк perf_issues и 100 алертов.
func TestDetectFingerprintNormalizesCulprit(t *testing.T) {
	fps := map[string]bool{}
	for i := 0; i < 100; i++ {
		tx := mkTx(1000, repeatSpans(6, "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 10)...)
		tx.Name = fmt.Sprintf("GET /users/%d", i)

		f := findingOf(t, Detect(tx, DefaultDetectorConfig()), KindNPlusOne)
		fps[f.Fingerprint] = true
		// Человекочитаемое имя остаётся сырым: его показывают в строке проблемы.
		if want := fmt.Sprintf("GET /users/%d", i); f.Culprit != want {
			t.Fatalf("Culprit = %q, want %q", f.Culprit, want)
		}
	}
	if len(fps) != 1 {
		t.Fatalf("фингерпринтов %d, want 1: нешаблонизированный маршрут плодит проблемы", len(fps))
	}
}

// Не только числовые id: ObjectID, хеши и slug'и с длинным числовым хвостом
// тоже обязаны схлопываться в один фингерпринт — иначе perf_issues растёт по
// строке на КАЖДЫЙ запрос (числовой маршрут это уже умел, остальные — нет).
func TestDetectFingerprintCollapsesIDLikeCulprits(t *testing.T) {
	names := []string{
		"GET /orders/5f8d0d55b54764421b7156c9",
		"GET /orders/5f8d0d55b54764421b7156ca",
		"GET /orders/64b7e1c2f0a9d3e4b5c6a7d8",
		"GET /orders/507f1f77bcf86cd799439011",
	}
	fps := map[string]bool{}
	for _, name := range names {
		tx := mkTx(1000, repeatSpans(6, "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 10)...)
		tx.Name = name
		f := findingOf(t, Detect(tx, DefaultDetectorConfig()), KindNPlusOne)
		fps[f.Fingerprint] = true
	}
	if len(fps) != 1 {
		t.Fatalf("фингерпринтов %d, want 1: маршруты с ObjectID плодят строки perf_issues", len(fps))
	}

	// Но РАЗНЫЕ эндпойнты остаются разными проблемами: маскирование не имеет
	// права склеить `/articles/hello-world` с `/checkout/confirm`.
	distinct := map[string]bool{}
	for _, name := range []string{"GET /articles/hello-world", "GET /checkout/confirm"} {
		tx := mkTx(1000, repeatSpans(6, "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 10)...)
		tx.Name = name
		f := findingOf(t, Detect(tx, DefaultDetectorConfig()), KindNPlusOne)
		distinct[f.Fingerprint] = true
	}
	if len(distinct) != 2 {
		t.Fatalf("фингерпринтов %d, want 2: разные эндпойнты склеились в одну проблему", len(distinct))
	}
}

// Транзакция без имени: проблему не к чему привязать, а culprit="" склеил бы
// несвязанные эндпойнты в одну проблему. Детекция пропускается.
func TestDetectSkipsUnnamedTransaction(t *testing.T) {
	tx := mkTx(1000, repeatSpans(6, "p1", "db.sql.query", "SELECT 1", 10)...)
	tx.Name = "  "
	if got := Detect(tx, DefaultDetectorConfig()); len(got) != 0 {
		t.Fatalf("транзакция без имени дала находки: %+v", got)
	}
}

// У N+1 есть пол по суммарному времени: 20 обращений в Redis по 0.2мс — это 4мс
// на весь запрос, будить из-за них человека нельзя.
func TestDetectNPlusOneTotalDurationFloor(t *testing.T) {
	chatter := make([]Span, 0, 20)
	for i := 0; i < 20; i++ {
		chatter = append(chatter, mkSpanUS(fmt.Sprintf("r%d", i), "p1", "db.redis", "GET config:global", 200))
	}
	if got := Detect(mkTx(40, chatter...), DefaultDetectorConfig()); len(got) != 0 {
		t.Fatalf("20 x 0.2мс redis (4мс всего) дали находку: %+v", got)
	}

	// Те же 20 запросов, но реальные 5мс к БД — это 100мс работы, и это N+1.
	real := repeatSpans(20, "p1", "db.sql.query", "SELECT * FROM users WHERE id = 1", 5)
	got := Detect(mkTx(200, real...), DefaultDetectorConfig())
	findingOf(t, got, KindNPlusOne)
}

// Здоровый параллельный веер (12 одновременных вызовов) — это ХОРОШО написанный
// эндпойнт, а не проблема. http_flood — про кандидата на распараллеливание,
// поэтому срабатывает только на последовательных вызовах.
func TestDetectHTTPFloodOnlySequential(t *testing.T) {
	parallel := make([]Span, 0, 12)
	for i := 0; i < 12; i++ {
		parallel = append(parallel, mkSpanAt(fmt.Sprintf("p%d", i), "root", "http.client",
			fmt.Sprintf("GET https://api.example.com/users/%d", i), 10, 900))
	}
	if got := Detect(mkTx(1000, parallel...), DefaultDetectorConfig()); len(got) != 0 {
		t.Fatalf("параллельный веер дал находки: %+v", got)
	}

	serial := make([]Span, 0, 12)
	for i := 0; i < 12; i++ {
		serial = append(serial, mkSpanAt(fmt.Sprintf("s%d", i), "root", "http.client",
			fmt.Sprintf("GET https://api.example.com/users/%d", i), i*50, 50))
	}
	f := findingOf(t, Detect(mkTx(600, serial...), DefaultDetectorConfig()), KindHTTPFlood)
	if got, want := f.Evidence["transaction_op"], "http.server"; got != want {
		t.Errorf("evidence transaction_op = %v, want %v", got, want)
	}
	if _, ok := f.Evidence["parent_op"]; ok {
		t.Error("evidence parent_op у http_flood: у лавины нет родителя, ключ переименован")
	}
}

// Пол по суммарному времени не имеет права ГЛУШИТЬ детектор у SDK со сломанными
// (нулевыми/обрезанными до миллисекунд) часами спанов: цикл из 40 обращений —
// проблема независимо от того, что SDK отчитался о нулевой длительности.
func TestDetectNPlusOneZeroClockSpans(t *testing.T) {
	// 40 спанов с нулевой длительностью: totalUS = 0, но count вдвое выше порога.
	loud := make([]Span, 0, 40)
	for i := 0; i < 40; i++ {
		loud = append(loud, mkSpanUS(fmt.Sprintf("z%d", i), "p1", "db.redis", "GET user:1", 0))
	}
	f := findingOf(t, Detect(mkTx(100, loud...), DefaultDetectorConfig()), KindNPlusOne)
	if got, want := f.Evidence["count"], 40; got != want {
		t.Fatalf("count = %v, want %v", got, want)
	}

	// А вот 6 спанов с нулевыми часами (чуть выше порога count и без времени) —
	// молчим: доказательств, что это дорого, нет никаких.
	quiet := make([]Span, 0, 6)
	for i := 0; i < 6; i++ {
		quiet = append(quiet, mkSpanUS(fmt.Sprintf("q%d", i), "p1", "db.redis", "GET user:1", 0))
	}
	if got := Detect(mkTx(100, quiet...), DefaultDetectorConfig()); len(got) != 0 {
		t.Fatalf("6 спанов с нулевыми часами дали находку: %+v", got)
	}
}

// Водопад с ОГРАНИЧЕННЫМ параллелизмом (20 вызовов по 2 одновременно) — такой же
// кандидат на распараллеливание, как чисто последовательный: доля стенного
// времени у него ~50%, и гейт isSequential его молча ронял.
func TestDetectHTTPFloodBoundedConcurrency(t *testing.T) {
	// 10 «волн» по 2 параллельных вызова: count/max_concurrency = 10 >= HTTPFloodMin.
	waves := make([]Span, 0, 20)
	for i := 0; i < 20; i++ {
		off := (i / 2) * 50
		waves = append(waves, mkSpanAt(fmt.Sprintf("w%d", i), "root", "http.client",
			fmt.Sprintf("GET https://api.example.com/users/%d", i), off, 50))
	}
	f := findingOf(t, Detect(mkTx(600, waves...), DefaultDetectorConfig()), KindHTTPFlood)
	if got, want := f.Evidence["max_concurrency"], 2; got != want {
		t.Errorf("evidence max_concurrency = %v, want %v", got, want)
	}
	if _, ok := f.Evidence["sequential"]; ok {
		t.Error("evidence sequential: гейт и так гарантирует истину, ключ бесполезен")
	}
	pct, ok := f.Evidence["sequential_pct"].(int)
	if !ok || pct < 40 || pct > 60 {
		t.Errorf("evidence sequential_pct = %v, want измеренную долю ~50", f.Evidence["sequential_pct"])
	}
}

// N+1 по кешу с БУКВЕННЫМИ идентификаторами (`user:jsmith`, `user:mbrown`) —
// такая же проблема, как с числовыми, и должна схлопываться в одну находку.
func TestDetectRedisNPlusOneAlphabeticKeys(t *testing.T) {
	spans := make([]Span, 0, 8)
	names := []string{"jsmith", "mbrown", "adoe", "kpetrov", "lgroshev", "nsmirnov", "pivanov", "rsidorov"}
	for i, n := range names {
		spans = append(spans, mkSpan(fmt.Sprintf("r%d", i), "p1", "db.redis", "GET user:"+n, 5))
	}
	f := findingOf(t, Detect(mkTx(200, spans...), DefaultDetectorConfig()), KindNPlusOne)
	if got, want := f.Evidence["count"], 8; got != want {
		t.Fatalf("count = %v, want %v: буквенные ключи не схлопнулись в одну группу", got, want)
	}
	if want := "GET user:?"; f.Description != want {
		t.Errorf("Description = %q, want %q", f.Description, want)
	}
}
