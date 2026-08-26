package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"
)

// Record — одна строка выгрузки: набор именованных значений. Допустимые типы
// значений: string, int64, bool, time.Time, json.RawMessage, nil.
type Record map[string]any

// Writer пишет записи в выбранный формат выгрузки. Реализации потоковые:
// данные уходят в нижележащий io.Writer по мере поступления записей, а не
// копятся в памяти до Close — заявки на выгрузку могут содержать миллионы строк.
type Writer interface {
	Write(Record) error
	Close() error
}

// NewWriter создаёт писателя нужного формата. columns задаёт порядок колонок
// для CSV; JSON и NDJSON пишут запись целиком и columns игнорируют.
func NewWriter(w io.Writer, f Format, columns []string) (Writer, error) {
	switch f {
	case FormatCSV:
		return newCSVWriter(w, columns)
	case FormatJSON:
		return newJSONWriter(w)
	case FormatNDJSON:
		return newNDJSONWriter(w), nil
	}
	return nil, fmt.Errorf("экспорт: неизвестный формат %q", f)
}

// csvSafe обезвреживает формульную инъекцию: Excel и LibreOffice исполняют
// значение, начинающееся с =, +, -, @ (а также с таба или CR — они съедаются
// парсером до символа-триггера). Текст ошибок пишет тот, кто шлёт события,
// поэтому выгрузка без этой защиты — готовый вектор атаки на того, кто её открыл.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// cell приводит значение к строке для CSV: время — всегда RFC3339 в UTC,
// чтобы файл не зависел от таймзоны того, кто его открыл.
func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case json.RawMessage:
		return string(x)
	}
	return fmt.Sprint(v)
}

// csvWriter пишет BOM и заголовок сразу при создании: они не зависят от
// данных, и ранний сбой нижележащего writer'а обнаруживается в NewWriter,
// а не только на первой строке. Flush после каждой строки — чтобы ошибка
// записи всплывала на той строке, где случилась, а не копилась до Close.
type csvWriter struct {
	cw      *csv.Writer
	columns []string
}

func newCSVWriter(w io.Writer, columns []string) (Writer, error) {
	if _, err := io.WriteString(w, "\ufeff"); err != nil {
		return nil, err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(columns); err != nil {
		return nil, err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	return &csvWriter{cw: cw, columns: columns}, nil
}

func (c *csvWriter) Write(rec Record) error {
	row := make([]string, len(c.columns))
	for i, col := range c.columns {
		row[i] = csvSafe(cell(rec[col]))
	}
	if err := c.cw.Write(row); err != nil {
		return err
	}
	c.cw.Flush()
	return c.cw.Error()
}

func (c *csvWriter) Close() error {
	c.cw.Flush()
	return c.cw.Error()
}

// jsonWriter пишет данные напрямую в нижележащий io.Writer записью за записью,
// без буферизации всего массива в памяти. Открывающая скобка уходит сразу
// в конструкторе, закрывающая — в Close; на нуле записей файл получается "[]".
type jsonWriter struct {
	w       io.Writer
	written int
}

func newJSONWriter(w io.Writer) (Writer, error) {
	if _, err := io.WriteString(w, "["); err != nil {
		return nil, err
	}
	return &jsonWriter{w: w}, nil
}

func (j *jsonWriter) Write(rec Record) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return err
	}
	data := bytes.TrimRight(buf.Bytes(), "\n")

	if j.written > 0 {
		if _, err := io.WriteString(j.w, ","); err != nil {
			return err
		}
	}
	if _, err := j.w.Write(data); err != nil {
		return err
	}
	j.written++
	return nil
}

func (j *jsonWriter) Close() error {
	_, err := io.WriteString(j.w, "]")
	return err
}

// ndjsonWriter — объект на строку, без буферизации: json.Encoder пишет прямо
// в нижележащий writer при каждом Encode, включая перевод строки после него.
type ndjsonWriter struct {
	enc *json.Encoder
}

func newNDJSONWriter(w io.Writer) Writer {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &ndjsonWriter{enc: enc}
}

func (n *ndjsonWriter) Write(rec Record) error {
	return n.enc.Encode(rec)
}

func (n *ndjsonWriter) Close() error {
	return nil
}
