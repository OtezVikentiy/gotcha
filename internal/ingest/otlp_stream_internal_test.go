package ingest

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// TestOTLPJSONHexIDsDoesNotAmplifyMemory — переписывание идентификаторов не
// материализует тело.
//
// Раньше otlpJSONHexIDs разбирала весь документ в map[string]any/[]any, и это
// была удалённая амплификация памяти: измерено 10 МиБ тела → 507 МиБ кучи
// (×51). На проводе с gzip такое тело весит ~15 КБ, а ключ приёма публичен по
// построению. Одного запроса хватало, чтобы положить процесс на профиле с
// mem_limit 256 МиБ.
//
// Порог ×4 выбран с запасом от честной стоимости (выходной буфер ≈ размер
// входа плюс рост при Grow), но втрое ниже прежней ×51 — так тест ловит именно
// возврат к материализации, а не колебания аллокатора.
func TestOTLPJSONHexIDsDoesNotAmplifyMemory(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for sb.Len() < 4<<20 {
		if sb.Len() > 1 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"a":1}`)
	}
	sb.WriteString("]")
	raw := []byte(sb.String())

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	out := otlpJSONHexIDs(raw)
	runtime.ReadMemStats(&m1)
	runtime.KeepAlive(out)

	grew := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	if grew < 0 {
		grew = 0
	}
	if ratio := float64(grew) / float64(len(raw)); ratio > 4 {
		t.Fatalf("амплификация ×%.1f (%d байт кучи на %d байт тела) — тело снова материализуется целиком",
			ratio, grew, len(raw))
	}
}

// TestOTLPJSONHexIDsPreservesDocument — потоковая перезапись обязана сохранять
// документ: структуру, точность чисел, юникод и символы, которые HTML-эскейп
// испортил бы. Идентификаторы при этом переводятся в base64.
func TestOTLPJSONHexIDsPreservesDocument(t *testing.T) {
	const body = `{"resourceSpans":[{"scopeSpans":[{"spans":[{` +
		`"traceId":"0123456789abcdef0123456789abcdef","spanId":"0123456789abcdef",` +
		`"parentSpanId":"fedcba9876543210","name":"GET /x?a=1&b=2","startTimeUnixNano":1750000000000000000,` +
		`"attributes":[{"key":"db.statement","value":{"stringValue":"SELECT * FROM t WHERE a < 3 AND b > 1"}},` +
		`{"key":"тег","value":{"stringValue":"значение"}}],` +
		`"ok":true,"nothing":null,"ratio":0.125}]}]}]}`

	out := otlpJSONHexIDs([]byte(body))
	got := string(out)

	// Идентификаторы переведены.
	for _, hexID := range []string{"0123456789abcdef0123456789abcdef", "fedcba9876543210"} {
		if strings.Contains(got, hexID) {
			t.Errorf("идентификатор %s остался в hex", hexID)
		}
	}
	// Наносекундный таймстемп не уехал в экспоненциальную запись.
	if !strings.Contains(got, "1750000000000000000") {
		t.Errorf("таймстемп потерял точность:\n%s", got)
	}
	// HTML-символы и юникод не тронуты.
	for _, want := range []string{"GET /x?a=1&b=2", "a < 3 AND b > 1", "тег", "значение", "0.125", "true", "null"} {
		if !strings.Contains(got, want) {
			t.Errorf("потеряно %q:\n%s", want, got)
		}
	}
	// Результат — валидный JSON той же формы.
	var before, after any
	if err := json.Unmarshal([]byte(body), &before); err != nil {
		t.Fatalf("исходное тело не разбирается: %v", err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatalf("переписанное тело не разбирается: %v\n%s", err, got)
	}
}

// TestOTLPJSONHexIDsLeavesNonHexAlone — тело без hex-идентификаторов должно
// возвращаться БАЙТ В БАЙТ: клиент, шлющий base64 (как protojson.Marshal),
// продолжает работать, и лишней работы мы не делаем.
func TestOTLPJSONHexIDsLeavesNonHexAlone(t *testing.T) {
	for _, body := range []string{
		`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"ASNFZ4mrze8BI0VniavN7w==","name":"x"}]}]}]}`,
		`{}`,
		`[]`,
		`{"a":  1,   "b" : "c"}`, // форматирование сохраняется, потому что тело не трогали
	} {
		if got := string(otlpJSONHexIDs([]byte(body))); got != body {
			t.Errorf("тело без hex-идентификаторов изменено:\nбыло:  %s\nстало: %s", body, got)
		}
	}
}

// TestOTLPJSONHexIDsRejectsGarbage — сломанное или лишнее содержимое отдаётся
// нетронутым: отчитаться об ошибке — задача protojson, а не наша.
func TestOTLPJSONHexIDsRejectsGarbage(t *testing.T) {
	for _, body := range []string{
		`{"traceId":`,
		`{"a":1} лишний хвост`,
		`not json at all`,
		``,
	} {
		if got := string(otlpJSONHexIDs([]byte(body))); got != body {
			t.Errorf("испорченное тело изменено:\nбыло:  %q\nстало: %q", body, got)
		}
	}
}
