package ingest

import (
	"encoding/json"
	"errors"
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
	out, err := otlpJSONHexIDs(raw)
	if err != nil {
		t.Fatalf("otlpJSONHexIDs: %v", err)
	}
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

	out, err := otlpJSONHexIDs([]byte(body))
	if err != nil {
		t.Fatalf("otlpJSONHexIDs: %v", err)
	}
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
		out, err := otlpJSONHexIDs([]byte(body))
		if err != nil {
			t.Fatalf("otlpJSONHexIDs(%q): %v", body, err)
		}
		if got := string(out); got != body {
			t.Errorf("тело без hex-идентификаторов изменено:\nбыло:  %s\nстало: %s", body, got)
		}
	}
}

// TestOTLPJSONHexIDsDepthBoundary — граница maxJSONWalkDepth ровная: значение
// глубины maxJSONWalkDepth проходит, maxJSONWalkDepth+1 — уже нет.
//
// Отсчёт глубины начинается с 0 у САМОГО ВЕРХНЕГО значения (см. вызов w.value(0)
// в otlpJSONHexIDs), а проверка в object/array — "depth > maxJSONWalkDepth". Тело
// из maxJSONWalkDepth+1 вложенных массивов ([[[...]]] с таким числом скобок)
// доходит максимум до глубины maxJSONWalkDepth (не больше) и проходит; на один
// уровень глубже — падает. Числа подобраны и проверены запуском, а не
// теоретически: голые счётные примеры вроде «глубина 100 / 101» здесь дали бы
// неверную границу ровно на единицу.
//
// Тело — голые скобки, а не валидный OTLP: этот тест проверяет только предел
// ОБХОДА (otlpJSONHexIDs), а не итоговый код ответа HTTP-эндпойнта — на голых
// скобках protojson всё равно отказал бы (это не объект), так что для сквозной
// проверки кода ответа используется валидное тело (см.
// TestOTLPJSONRejectsDeepBody / TestOTLPJSONAcceptsNormalDepth в otlp_depth_test.go).
func TestOTLPJSONHexIDsDepthBoundary(t *testing.T) {
	atLimit := strings.Repeat("[", maxJSONWalkDepth+1) + strings.Repeat("]", maxJSONWalkDepth+1)
	if _, err := otlpJSONHexIDs([]byte(atLimit)); err != nil {
		t.Errorf("глубина %d (на пределе): err = %v, want nil", maxJSONWalkDepth+1, err)
	}

	overLimit := strings.Repeat("[", maxJSONWalkDepth+2) + strings.Repeat("]", maxJSONWalkDepth+2)
	if _, err := otlpJSONHexIDs([]byte(overLimit)); !errors.Is(err, errJSONTooDeep) {
		t.Errorf("глубина %d (за пределом): err = %v, want errJSONTooDeep", maxJSONWalkDepth+2, err)
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
		out, err := otlpJSONHexIDs([]byte(body))
		if err != nil {
			t.Fatalf("otlpJSONHexIDs(%q): %v", body, err)
		}
		if got := string(out); got != body {
			t.Errorf("испорченное тело изменено:\nбыло:  %q\nстало: %q", body, got)
		}
	}
}
