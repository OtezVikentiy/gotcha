package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"slices"
	"sort"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

// Тесты ниже — сторож K4-7 аудита в его исправленной постановке: находка не
// про поля структуры Meta саму по себе, а про формат выгрузки ЦЕЛИКОМ —
// MetaSchemaVersion (meta.go) обязана двигаться при ЛЮБОМ несовместимом
// изменении набора колонок/ключей файла (CSV/JSON/NDJSON), не только полей
// Meta. Иначе потребитель узнал бы о переименованной колонке только
// сломанным парсингом, при том что version у него на руках не изменилась.
//
// Источник «истины» в каждом тесте — РЕАЛЬНЫЙ вывод писателя на фикстуре:
// маппинг источника (eventSource.toRecord/issueSource.toRecord — та же
// функция, что зовёт настоящий Stream) → NewWriter(...).Write(...).Close() →
// разбор получившихся байт тем же способом, каким их читает настоящий
// потребитель (encoding/csv, encoding/json). Сравнение — с ЛИТЕРАЛОМ,
// набранным в тесте руками, а НЕ с повторным вызовом EventColumns()/
// IssueColumns(): если источником «ожидания» была бы та же функция, что и
// источником «факта», переименование колонки сдвинуло бы оба одним и тем же
// изменением одной строки, и тест остался бы зелёным при реальном изменении
// контракта (см. TestEventColumnsContractPin/TestIssueColumnsContractPin —
// тот же принцип, только для одного лишь CSV-заголовка; тесты этого файла
// покрывают контракт целиком, все три формата, оба вида заявки).
const contractBreakMsg = "%s %s: набор ключей = %v, want %v — это ломающее изменение контракта выгрузки, подними MetaSchemaVersion (meta.go)"

func fixtureStoredEventForContract() event.Stored {
	return event.Stored{
		ID:             "ev-1",
		IssueID:        1,
		Timestamp:      time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		Level:          "error",
		Message:        "m",
		ExceptionType:  "T",
		ExceptionValue: "V",
		Stacktrace:     `{"values":[]}`,
		Environment:    "prod",
		Release:        "1.0",
		ServerName:     "host",
		SDK:            "go",
		UserID:         "u1",
		UserIP:         "1.2.3.4",
		UserEmail:      "a@b.com",
		Tags:           map[string]string{"k": "v"},
		Contexts:       `{}`,
		Breadcrumbs:    `[]`,
		Request:        `{}`,
		TraceID:        "tr-1",
	}
}

func fixtureIssueForContract() issue.Issue {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	return issue.Issue{
		ID:            1,
		Title:         "t",
		Culprit:       "c",
		Level:         "error",
		Status:        "unresolved",
		TimesSeen:     3,
		FirstSeen:     now,
		LastSeen:      now,
		Environments:  []string{"prod"},
		AssigneeEmail: "a@b.com",
	}
}

// writeOneRecord прогоняет rec через настоящий Writer формата f и отдаёт
// сырые байты — то же самое, что получил бы потребитель, скачавший файл.
func writeOneRecord(t *testing.T, f Format, columns []string, rec Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, f, columns)
	if err != nil {
		t.Fatalf("NewWriter(%s): %v", f, err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatalf("Write(%s): %v", f, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close(%s): %v", f, err)
	}
	return buf.Bytes()
}

// csvHeader разбирает заголовок настоящего CSV-вывода (BOM пропускается).
func csvHeader(t *testing.T, raw []byte) []string {
	t.Helper()
	got, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\ufeff")))).Read()
	if err != nil {
		t.Fatalf("csv.Read заголовка: %v (%s)", err, raw)
	}
	return got
}

// jsonRecordKeys разбирает единственный элемент JSON-массива и отдаёт его
// ключи отсортированными (порядок ключей у map при кодировании и так
// сортируется encoding/json, сортировка здесь — для устойчивости теста).
func jsonRecordKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("json.Unmarshal(JSON): %v (%s)", err, raw)
	}
	if len(arr) != 1 {
		t.Fatalf("элементов JSON-массива = %d, want 1 (%s)", len(arr), raw)
	}
	return sortedKeys(arr[0])
}

// ndjsonRecordKeys разбирает единственную строку NDJSON.
func ndjsonRecordKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	line := bytes.TrimRight(raw, "\n")
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("json.Unmarshal(NDJSON): %v (%s)", err, raw)
	}
	return sortedKeys(m)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestExportContractEventFieldsMatchFrozenSet — контракт выгрузки events по
// всем трём форматам. CSV несёт подмножество колонок (EventColumns, §6
// спеки, порядок значим — CSV-писатель кладёт значения позиционно);
// JSON/NDJSON пишут Record целиком, это подмножество ПЛЮС
// stacktrace/contexts/breadcrumbs/request (см. докблок EventColumns) —
// ключи, множество без порядка.
func TestExportContractEventFieldsMatchFrozenSet(t *testing.T) {
	csvWant := []string{"timestamp", "event_id", "issue_id", "level", "message",
		"exception_type", "exception_value", "environment", "release", "server_name",
		"sdk", "trace_id", "user_id", "user_ip", "user_email", "tags"}
	fullWant := append(append([]string{}, csvWant...), "breadcrumbs", "contexts", "request", "stacktrace")
	sort.Strings(fullWant)

	rec := (&eventSource{}).toRecord(fixtureStoredEventForContract(), true, nil)

	if got := csvHeader(t, writeOneRecord(t, FormatCSV, EventColumns(), rec)); !slices.Equal(got, csvWant) {
		t.Fatalf(contractBreakMsg, "CSV", "events", got, csvWant)
	}
	if got := jsonRecordKeys(t, writeOneRecord(t, FormatJSON, nil, rec)); !slices.Equal(got, fullWant) {
		t.Fatalf(contractBreakMsg, "JSON", "events", got, fullWant)
	}
	if got := ndjsonRecordKeys(t, writeOneRecord(t, FormatNDJSON, nil, rec)); !slices.Equal(got, fullWant) {
		t.Fatalf(contractBreakMsg, "NDJSON", "events", got, fullWant)
	}
}

// TestExportContractIssueFieldsMatchFrozenSet — контракт выгрузки issues по
// всем трём форматам. В отличие от events, у issues Record не несёт полей
// сверх IssueColumns() (докблок IssueColumns: «JSON/NDJSON пишут Record
// целиком и порядок игнорируют») — набор ключей одинаков во всех форматах,
// различается только то, важен ли порядок (CSV — да, JSON/NDJSON — нет).
func TestExportContractIssueFieldsMatchFrozenSet(t *testing.T) {
	want := []string{"id", "title", "culprit", "level", "status", "times_seen",
		"first_seen", "last_seen", "environments", "assignee_email", "url"}
	wantSorted := append([]string{}, want...)
	sort.Strings(wantSorted)

	rec := (&issueSource{baseURL: "https://gotcha.example.com"}).toRecord(fixtureIssueForContract(), true)

	if got := csvHeader(t, writeOneRecord(t, FormatCSV, IssueColumns(), rec)); !slices.Equal(got, want) {
		t.Fatalf(contractBreakMsg, "CSV", "issues", got, want)
	}
	if got := jsonRecordKeys(t, writeOneRecord(t, FormatJSON, nil, rec)); !slices.Equal(got, wantSorted) {
		t.Fatalf(contractBreakMsg, "JSON", "issues", got, wantSorted)
	}
	if got := ndjsonRecordKeys(t, writeOneRecord(t, FormatNDJSON, nil, rec)); !slices.Equal(got, wantSorted) {
		t.Fatalf(contractBreakMsg, "NDJSON", "issues", got, wantSorted)
	}
}

// TestExportContractMetaFieldsMatchFrozenSet — тот же принцип, что у двух
// тестов выше, применённый к самой структуре Meta (K4-7 аудита):
// MetaSchemaVersion объявляет несовместимой правкой переименование/удаление
// поля Meta, но без сторожа на ПОЛНЫЙ набор ключей это обещание в докблоке
// ничем не удержано — переименуй FilterCode в структуре, и ни один из
// прежних тестов (TestBuildMetaAlwaysSetsSchemaVersion,
// TestMetaSchemaVersionFieldNameAndValue — оба смотрят только на
// schema_version) не покраснеет.
//
// pseudonym_note — единственное опциональное поле (`omitempty`, докблок
// PseudonymNote: непусто только у events без ПДн) — тест проверяет оба
// состояния явно, а не только «набор ключей достаточно большой»: три
// обязательных ключа присутствуют ВСЕГДА, pseudonym_note — РОВНО там, где
// докблок его обещает, и нигде больше.
func TestExportContractMetaFieldsMatchFrozenSet(t *testing.T) {
	alwaysWant := []string{"filter_code", "schema_version", "scope_issue_id"}

	cases := []struct {
		name         string
		job          Job
		wantOptional []string
	}{
		{"issues", Job{Kind: KindIssues}, nil},
		{"events, includePII=true", Job{Kind: KindEvents, IncludePII: true}, nil},
		{"events, includePII=false", Job{Kind: KindEvents, IncludePII: false}, []string{"pseudonym_note"}},
	}
	for _, c := range cases {
		raw, err := json.Marshal(BuildMeta(c.job))
		if err != nil {
			t.Fatalf("%s: json.Marshal: %v", c.name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: json.Unmarshal: %v (%s)", c.name, err, raw)
		}
		want := append(append([]string{}, alwaysWant...), c.wantOptional...)
		sort.Strings(want)
		if got := sortedKeys(m); !slices.Equal(got, want) {
			t.Fatalf(contractBreakMsg, "Meta JSON", c.name, got, want)
		}
	}
}
