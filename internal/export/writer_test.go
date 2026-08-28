package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCSVWriterBOMHeaderAndTime(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, FormatCSV, []string{"id", "title", "last_seen"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ts := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	if err := w.Write(Record{"id": int64(7), "title": "боль", "last_seen": ts}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "\ufeff") {
		t.Error("нет BOM: Excel сломает кириллицу")
	}
	if !strings.Contains(out, "id,title,last_seen") {
		t.Errorf("нет строки заголовков: %q", out)
	}
	if !strings.Contains(out, "2026-08-26T09:30:00Z") {
		t.Errorf("время не в RFC3339 UTC: %q", out)
	}
}

// Значения событий контролирует тот, кто их шлёт: сообщение вида =cmd|'/c calc'!A1
// в CSV исполняется Excel'ем как формула. Префикс апострофа обязателен.
func TestCSVWriterEscapesFormulas(t *testing.T) {
	for _, dangerous := range []string{"=1+1", "+1", "-1", "@SUM(A1)", "\t=1", "\r=1"} {
		var buf bytes.Buffer
		w, _ := NewWriter(&buf, FormatCSV, []string{"message"})
		if err := w.Write(Record{"message": dangerous}); err != nil {
			t.Fatalf("Write(%q): %v", dangerous, err)
		}
		w.Close()
		line := strings.Split(strings.TrimPrefix(buf.String(), "\ufeff"), "\n")[1]
		if !strings.HasPrefix(strings.TrimPrefix(line, `"`), "'") {
			t.Errorf("значение %q не обезврежено: строка %q", dangerous, line)
		}
	}
}

// Пустая строка не должна получать апостроф — обезвреживать нечего, а лишний
// символ испортил бы легитимные данные.
func TestCSVSafeEmptyUnchanged(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatCSV, []string{"message"})
	if err := w.Write(Record{"message": ""}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.Close()
	line := strings.Split(strings.TrimPrefix(buf.String(), "\ufeff"), "\n")[1]
	if strings.HasPrefix(line, "'") {
		t.Errorf("пустое значение обезврежено зря: %q", line)
	}
}

// Безопасное значение (не начинается с триггера формулы) обязано пройти без
// изменений — иначе выгрузка искажает обычные данные.
func TestCSVSafeOrdinaryValueUnchanged(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatCSV, []string{"message"})
	if err := w.Write(Record{"message": "обычный текст"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.Close()
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), "\ufeff")))
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got := rows[1][0]; got != "обычный текст" {
		t.Errorf("значение испорчено: %q", got)
	}
}

// Запятая, кавычка и перевод строки внутри значения обязаны пережить
// round-trip через RFC4180-парсер без потери и без расползания по колонкам.
// csv.Reader сам нормализует одинокий \r\n внутри поля в \n (поведение
// парсера, не писателя) — сравниваем с уже нормализованным ожиданием.
func TestCSVWriterEscapesSpecialChars(t *testing.T) {
	cases := []struct{ in, want string }{
		{`значение, с запятой`, `значение, с запятой`},
		{`значение "в кавычках"`, `значение "в кавычках"`},
		{"значение\nс переводом строки", "значение\nс переводом строки"},
		{"значение\r\nс CRLF", "значение\nс CRLF"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		w, _ := NewWriter(&buf, FormatCSV, []string{"a", "b"})
		if err := w.Write(Record{"a": tc.in, "b": "хвост"}); err != nil {
			t.Fatalf("Write(%q): %v", tc.in, err)
		}
		w.Close()
		r := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), "\ufeff")))
		rows, err := r.ReadAll()
		if err != nil {
			t.Fatalf("значение %q: ReadAll: %v", tc.in, err)
		}
		if len(rows) != 2 || len(rows[1]) != 2 {
			t.Fatalf("значение %q: разъехались колонки: %#v", tc.in, rows)
		}
		if rows[1][0] != tc.want {
			t.Errorf("значение %q исказилось при round-trip: получили %q, ожидали %q", tc.in, rows[1][0], tc.want)
		}
		if rows[1][1] != "хвост" {
			t.Errorf("значение %q сдвинуло соседнюю колонку: %q", tc.in, rows[1][1])
		}
	}
}

// Отсутствующий в записи ключ — это nil, а не паника: cell(nil) обязан дать
// пустую строку, не "<nil>".
func TestCSVWriterMissingFieldIsEmpty(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatCSV, []string{"id", "missing"})
	if err := w.Write(Record{"id": int64(1)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.Close()
	line := strings.Split(strings.TrimPrefix(buf.String(), "\ufeff"), "\n")[1]
	if line != "1," {
		t.Errorf("пустое поле сериализовано неверно: %q", line)
	}
}

// Очень длинное значение не должно обрезаться писателем — усечение решает
// вызывающий код (по бюджету строк), а не формат.
func TestCSVWriterLongValueNotTruncated(t *testing.T) {
	long := strings.Repeat("x", 100_000)
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatCSV, []string{"message"})
	if err := w.Write(Record{"message": long}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.Close()
	if !strings.Contains(buf.String(), long) {
		t.Error("длинное значение обрезано")
	}
}

// bool и NUL-байт внутри строки не должны ронять писателя.
func TestCSVWriterBoolAndNUL(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatCSV, []string{"ok", "raw"})
	if err := w.Write(Record{"ok": true, "raw": "до\x00после"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(buf.String(), "true") {
		t.Errorf("bool не сериализован: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "до\x00после") {
		t.Errorf("NUL-байт потерян: %q", buf.String())
	}
}

func TestJSONWriterValidArray(t *testing.T) {
	for _, n := range []int{0, 1, 3} {
		var buf bytes.Buffer
		w, _ := NewWriter(&buf, FormatJSON, []string{"id"})
		for i := 0; i < n; i++ {
			if err := w.Write(Record{"id": int64(i), "stacktrace": json.RawMessage(`{"frames":[]}`)}); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		var got []map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("n=%d: невалидный JSON %q: %v", n, buf.String(), err)
		}
		if len(got) != n {
			t.Errorf("n=%d: в массиве %d объектов", n, len(got))
		}
		if n > 0 {
			if _, ok := got[0]["stacktrace"].(map[string]any); !ok {
				t.Errorf("stacktrace попал строкой, а не вложенным объектом: %#v", got[0]["stacktrace"])
			}
		}
	}
}

// Байтовая проверка нулевой выгрузки: файл обязан быть буквально "[]", а не
// пустой строкой и не "[" без закрытия.
func TestJSONWriterEmptyIsBracketPair(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatJSON, nil)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := buf.String(); got != "[]" {
		t.Errorf("пустой JSON-массив: получили %q, ожидали \"[]\"", got)
	}
}

// Разделитель между объектами — запятая ровно между ними, не перед первым и
// не после последнего: округлый round-trip через Unmarshal это маскирует.
func TestJSONWriterCommaPlacement(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatJSON, nil)
	w.Write(Record{"id": int64(1)})
	w.Write(Record{"id": int64(2)})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := buf.String()
	if strings.HasPrefix(out, "[,") || strings.HasSuffix(out, ",]") {
		t.Errorf("лишняя запятая на границе массива: %q", out)
	}
	if strings.Count(out, ",") != 1 {
		t.Errorf("ожидали ровно одну запятую между двумя объектами: %q", out)
	}
}

// Энкодер по умолчанию экранирует <, >, & в \uXXXX — для выгружаемого файла
// (не HTML-контекст) это не нужно и портит читаемость сообщений об ошибках.
func TestJSONWriterDoesNotHTMLEscape(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatJSON, nil)
	if err := w.Write(Record{"message": "if (a < b && b > c) { x() }"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := buf.String()
	for _, escaped := range []string{`\u003c`, `\u0026`, `\u003e`} {
		if strings.Contains(out, escaped) {
			t.Errorf("символ экранирован в юникод-последовательность %s вместо литерала: %q", escaped, out)
		}
	}
	if !strings.Contains(out, "a < b && b > c") {
		t.Errorf("оригинальные символы потеряны или испорчены: %q", out)
	}
}

// Невалидный UTF-8 не должен ронять запись и обязан дать валидный JSON на
// выходе (json.Marshal подменяет невалидные байты U+FFFD).
func TestJSONWriterInvalidUTF8(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatJSON, nil)
	bad := "до\xffпосле"
	if err := w.Write(Record{"raw": bad}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("невалидный JSON на выходе: %q: %v", buf.String(), err)
	}
}

func TestNDJSONWriterOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatNDJSON, []string{"id"})
	for i := 0; i < 3; i++ {
		w.Write(Record{"id": int64(i)})
	}
	w.Close()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("строк %d, ожидали 3: %q", len(lines), buf.String())
	}
	for i, ln := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Errorf("строка %d невалидна: %v", i, err)
		}
	}
}

// Файл обязан заканчиваться переводом строки и после последнего объекта —
// это то, что отличает NDJSON от JSON Lines без хвостового \n на некоторых
// потребителях, которые читают построчно через bufio.Scanner.
func TestNDJSONWriterTrailingNewline(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatNDJSON, nil)
	w.Write(Record{"id": int64(1)})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("нет перевода строки в конце: %q", buf.String())
	}
}

func TestNewWriterUnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewWriter(&buf, Format("xml"), nil); err == nil {
		t.Error("ожидали ошибку на неизвестном формате")
	}
}

// countingWriter считает вызовы Write — им проверяем, что писатель отдаёт
// данные наружу по мере поступления записей, а не копит всё до Close.
type countingWriter struct {
	buf   bytes.Buffer
	calls int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.calls++
	return c.buf.Write(p)
}

func TestJSONWriterStreamsIncrementally(t *testing.T) {
	cw := &countingWriter{}
	w, _ := NewWriter(cw, FormatJSON, nil)
	callsAfterOpen := cw.calls
	for i := 0; i < 5; i++ {
		if err := w.Write(Record{"id": int64(i)}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	callsBeforeClose := cw.calls
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if callsBeforeClose <= callsAfterOpen {
		t.Errorf("записи не долетели до Close: было %d вызовов после открытия, %d перед закрытием", callsAfterOpen, callsBeforeClose)
	}
	if cw.calls <= callsBeforeClose {
		t.Errorf("Close ничего не дописал (нет закрывающей скобки): %d -> %d", callsBeforeClose, cw.calls)
	}
}

func TestNDJSONWriterStreamsIncrementally(t *testing.T) {
	cw := &countingWriter{}
	w, _ := NewWriter(cw, FormatNDJSON, nil)
	for i := 0; i < 5; i++ {
		if err := w.Write(Record{"id": int64(i)}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if cw.calls < 5 {
		t.Errorf("записи не пишутся по одной: %d вызовов на 5 записей", cw.calls)
	}
	w.Close()
}

func TestCSVWriterStreamsIncrementally(t *testing.T) {
	cw := &countingWriter{}
	w, _ := NewWriter(cw, FormatCSV, []string{"id"})
	callsAfterHeader := cw.calls
	if callsAfterHeader == 0 {
		t.Fatal("заголовок не дошёл до нижележащего writer'а сразу")
	}
	for i := 0; i < 3; i++ {
		if err := w.Write(Record{"id": int64(i)}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if cw.calls <= callsAfterHeader {
		t.Error("строки не дошли до нижележащего writer'а до Close")
	}
	w.Close()
}

// failingWriter пропускает ровно budget байт, дальше любая запись — ошибка.
// Байтовый, а не количественный лимит: тест не завязан на то, сколькими
// вызовами Write() писатель решит раздробить свои данные.
type failingWriter struct {
	buf    bytes.Buffer
	budget int
}

var errWriteFailed = errors.New("нижележащий writer отказал")

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.budget <= 0 {
		return 0, errWriteFailed
	}
	if len(p) > f.budget {
		n, _ := f.buf.Write(p[:f.budget])
		f.budget = 0
		return n, errWriteFailed
	}
	n, err := f.buf.Write(p)
	f.budget -= n
	return n, err
}

func TestCSVWriterWriteErrorPropagates(t *testing.T) {
	var probe bytes.Buffer
	w0, err := NewWriter(&probe, FormatCSV, []string{"id"})
	if err != nil {
		t.Fatalf("NewWriter (проба): %v", err)
	}
	if err := w0.Close(); err != nil {
		t.Fatalf("Close (проба): %v", err)
	}
	headerLen := probe.Len()

	fw := &failingWriter{budget: headerLen}
	w, err := NewWriter(fw, FormatCSV, []string{"id"})
	if err != nil {
		t.Fatalf("NewWriter в пределах бюджета не должен падать: %v", err)
	}
	if err := w.Write(Record{"id": int64(1)}); err == nil {
		t.Error("ожидали ошибку записи строки, получили nil — ошибка нижележащего writer'а проглочена")
	}
}

func TestCSVWriterConstructorErrorPropagates(t *testing.T) {
	fw := &failingWriter{budget: 0}
	if _, err := NewWriter(fw, FormatCSV, []string{"id"}); err == nil {
		t.Error("ожидали ошибку уже на BOM/заголовке, получили nil")
	}
}

// Длинное значение (100КБ) не помещается в буфер encoding/csv целиком:
// ошибка нижележащего writer'а всплывает прямо из c.cw.Write(row), а не
// только из Flush()+Error() — маленький бюджет (20 байт) пропускает
// BOM и заголовок, а на самой длинной строке отказ происходит сразу.
func TestCSVWriterRowWriteErrorOnLargeField(t *testing.T) {
	fw := &failingWriter{budget: 20}
	w, err := NewWriter(fw, FormatCSV, []string{"message"})
	if err != nil {
		t.Fatalf("NewWriter в пределах бюджета не должен падать: %v", err)
	}
	long := strings.Repeat("x", 100_000)
	if err := w.Write(Record{"message": long}); err == nil {
		t.Error("ожидали ошибку записи длинной строки, получили nil — ошибка c.cw.Write(row) проглочена")
	}
}

func TestJSONWriterWriteErrorPropagates(t *testing.T) {
	fw := &failingWriter{budget: 1} // ровно на "["
	w, err := NewWriter(fw, FormatJSON, nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Write(Record{"id": int64(1)}); err == nil {
		t.Error("ожидали ошибку записи, получили nil")
	}
}

func TestJSONWriterCloseErrorPropagates(t *testing.T) {
	var probe bytes.Buffer
	w0, _ := NewWriter(&probe, FormatJSON, nil)
	w0.Write(Record{"id": int64(1)})
	budget := probe.Len()

	fw := &failingWriter{budget: budget}
	w, err := NewWriter(fw, FormatJSON, nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Write(Record{"id": int64(1)}); err != nil {
		t.Fatalf("Write в пределах бюджета не должен падать: %v", err)
	}
	if err := w.Close(); err == nil {
		t.Error("ожидали ошибку на закрывающей скобке, получили nil")
	}
}

func TestNDJSONWriterWriteErrorPropagates(t *testing.T) {
	fw := &failingWriter{budget: 0}
	w, err := NewWriter(fw, FormatNDJSON, nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Write(Record{"id": int64(1)}); err == nil {
		t.Error("ожидали ошибку записи, получили nil")
	}
}

// При ошибке на N-й записи частичный файл не обязан выглядеть валидным целым:
// незакрытый JSON-массив это гарантирует сам по себе.
func TestJSONWriterPartialOutputNotValidOnError(t *testing.T) {
	fw := &failingWriter{budget: 1} // ровно на "["
	w, err := NewWriter(fw, FormatJSON, nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Write(Record{"id": int64(1)}); err == nil {
		t.Fatal("ожидали ошибку записи первой записи")
	}
	var got []map[string]any
	if err := json.Unmarshal(fw.buf.Bytes(), &got); err == nil {
		t.Errorf("частичный вывод неожиданно валиден: %q", fw.buf.String())
	}
}

// Тип за пределами документированного контракта (string/int64/bool/
// time.Time/json.RawMessage/nil) не должен паниковать — cell() обязан дать
// хоть какое-то текстовое представление через запасной путь.
func TestCellUnknownTypeFallback(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatCSV, []string{"weird"})
	if err := w.Write(Record{"weird": float64(3.5)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.Close()
	if !strings.Contains(buf.String(), "3.5") {
		t.Errorf("нестандартный тип не сериализован запасным путём: %q", buf.String())
	}
}

// Ошибка при записи строки заголовков (после успешного BOM) обязана
// всплывать из newCSVWriter, а не только ошибка на самом BOM.
func TestCSVWriterHeaderWriteErrorPropagates(t *testing.T) {
	var probe bytes.Buffer
	if _, err := io.WriteString(&probe, "\ufeff"); err != nil {
		t.Fatalf("io.WriteString: %v", err)
	}
	bomLen := probe.Len()

	fw := &failingWriter{budget: bomLen}
	if _, err := NewWriter(fw, FormatCSV, []string{"id"}); err == nil {
		t.Error("ожидали ошибку записи заголовка, получили nil")
	}
}

// Ошибка на самой открывающей скобке "[" обязана всплывать из NewWriter,
// а не только ошибка на записи первого объекта.
func TestJSONWriterConstructorErrorPropagates(t *testing.T) {
	fw := &failingWriter{budget: 0}
	if _, err := NewWriter(fw, FormatJSON, nil); err == nil {
		t.Error("ожидали ошибку записи \"[\", получили nil")
	}
}

// Значение, которое encoding/json не умеет сериализовать (канал), обязано
// дать ошибку из Write, а не панику и не молчаливый пропуск записи.
func TestJSONWriterEncodeErrorPropagates(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatJSON, nil)
	if err := w.Write(Record{"bad": make(chan int)}); err == nil {
		t.Error("ожидали ошибку сериализации нестандартного значения, получили nil")
	}
}

// cell() отдельно от JSON-писателей — единственный потребитель ветки
// json.RawMessage: raw-значение должно попасть в CSV как есть, без
// повторного экранирования кавычек внутри уже готового JSON-фрагмента.
func TestCellRawMessageInCSV(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, FormatCSV, []string{"raw"})
	if err := w.Write(Record{"raw": json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.Close()
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), "\ufeff")))
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if rows[1][0] != `{"a":1}` {
		t.Errorf("json.RawMessage испорчен в CSV: %q", rows[1][0])
	}
}

// Разделительная запятая перед вторым объектом — отдельная точка отказа от
// записи самих данных: бюджет пропускает ровно первый объект целиком.
func TestJSONWriterCommaWriteErrorPropagates(t *testing.T) {
	var probe bytes.Buffer
	w0, _ := NewWriter(&probe, FormatJSON, nil)
	w0.Write(Record{"id": int64(1)})
	budget := probe.Len()

	fw := &failingWriter{budget: budget}
	w, err := NewWriter(fw, FormatJSON, nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Write(Record{"id": int64(1)}); err != nil {
		t.Fatalf("первая запись в пределах бюджета не должна падать: %v", err)
	}
	if err := w.Write(Record{"id": int64(2)}); err == nil {
		t.Error("ожидали ошибку на разделительной запятой перед вторым объектом")
	}
}

// TestCSVWriterReadableByStandardCSVReader — F5/F1′ контрактной уборки
// 2026-08-28 (CONTRACT-DECISIONS.md): CSV-файл выгрузки обязан парситься
// СТАНДАРТНЫМ encoding/csv.Reader с настройками по умолчанию — тот обязывает
// все строки нести ОДИНАКОВОЕ число полей, начиная с первой прочитанной.
// Именно эта проверка поймала регресс предыдущего прохода (P1 ревью):
// комментарий "# gotcha-export-meta ..." перед строкой колонок ломал файл
// РОВНО так — у комментария 1 поле (в нём нет запятых), у настоящей строки
// колонок — len(columns), и csv.Reader падал "record on line 2: wrong number
// of fields", а не просто "не то содержимое". Мутационная точка F5: вернуть
// комментарий метаданных перед строкой колонок в newCSVWriter (writer.go) —
// этот тест обязан упасть на err от r.ReadAll(), а не молча пройти.
//
// BOM перед первым полем не срезаем нарочно избирательно — это конвенция
// формата (Excel опознаёт кодировку только по нему, см. docblock newCSVWriter),
// а не дефект: срез делает сам тест перед сравнением, тем же приёмом, каким
// это делает любой типичный потребитель (Python — encoding="utf-8-sig").
func TestCSVWriterReadableByStandardCSVReader(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, FormatCSV, []string{"id", "title"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Write(Record{"id": int64(1), "title": "boom"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Write(Record{"id": int64(2), "title": "bang"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r := csv.NewReader(bytes.NewReader(buf.Bytes()))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("encoding/csv (настройки по умолчанию) не разобрал файл: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("получено %d строк, want 3 (заголовок + 2 строки данных): %v", len(records), records)
	}
	header := records[0]
	header[0] = strings.TrimPrefix(header[0], "\ufeff")
	if header[0] != "id" || header[1] != "title" {
		t.Errorf("первая прочитанная строка не является строкой колонок: %v", header)
	}
	if records[1][0] != "1" || records[1][1] != "boom" {
		t.Errorf("вторая строка = %v, want [1 boom]", records[1])
	}
	if records[2][0] != "2" || records[2][1] != "bang" {
		t.Errorf("третья строка = %v, want [2 bang]", records[2])
	}
}

// TestJSONWriterDecodesDirectlyIntoRecordSlice — F5: получатель обязан
// разбирать файл наивным json.Unmarshal(data, &[]map[string]any{}) — без
// какой-либо специальной обработки первого элемента. Именно эта проверка
// поймала регресс предыдущего прохода: элемент {"_export_meta": {...}}
// первым в массиве не был строкой данных, и код вида
// `for _, row := range rows { _ = row["id"].(int64) }` (Go) или
// `for row in json.load(f): row["id"]` (Python) падал бы на элементе 0.
// Мутационная точка F5: вернуть writeMeta/exportMetaElement первым элементом
// в newJSONWriter (writer.go) — len(rows) станет 3 вместо 2 и rows[0]["id"]
// не будет float64(1): оба Errorf ниже обязаны упасть.
func TestJSONWriterDecodesDirectlyIntoRecordSlice(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, FormatJSON, nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Write(Record{"id": int64(1)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Write(Record{"id": int64(2)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("json.Unmarshal без спецобработки первого элемента: %v (%q)", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("получено %d элементов, want 2 (только строки данных, без служебного первого): %v", len(rows), rows)
	}
	if got, want := rows[0]["id"], float64(1); got != want {
		t.Errorf("rows[0][%q] = %v, want %v — первый элемент обязан быть данными, не метаданными", "id", got, want)
	}
	if got, want := rows[1]["id"], float64(2); got != want {
		t.Errorf("rows[1][%q] = %v, want %v", "id", got, want)
	}
}

// TestNDJSONWriterEachLineIsHomogeneousRecord — F5: тот же контракт, что и
// у JSON-массива выше, но построчно — ни одна строка NDJSON-файла не несёт
// служебный ключ "_export_meta", каждая декодируется в ту же форму записи.
func TestNDJSONWriterEachLineIsHomogeneousRecord(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, FormatNDJSON, nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Write(Record{"id": int64(1)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Write(Record{"id": int64(2)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("получено %d строк, want 2 (только строки данных): %q", len(lines), buf.String())
	}
	wantIDs := []float64{1, 2}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("строка %d: json.Unmarshal: %v (%q)", i, err, line)
		}
		if _, ok := row["_export_meta"]; ok {
			t.Errorf("строка %d несёт служебный ключ _export_meta: %v", i, row)
		}
		if got := row["id"]; got != wantIDs[i] {
			t.Errorf("строка %d: id = %v, want %v", i, got, wantIDs[i])
		}
	}
}
