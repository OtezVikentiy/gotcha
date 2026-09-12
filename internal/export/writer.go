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

// Допустимые типы значений: string, int64, bool, time.Time, json.RawMessage, nil.
type Record map[string]any

// Реализации потоковые: данные уходят в io.Writer по мере поступления, не копятся в памяти до Close.
type Writer interface {
	Write(Record) error
	Close() error
}

// columns задаёт порядок для CSV; JSON/NDJSON пишут запись целиком и columns игнорируют.
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

// Обезвреживает формульную инъекцию: Excel/LibreOffice исполняют значение, начинающееся с =,+,-,@
// (и с таба/CR — они съедаются парсером до триггера). Текст ошибок пишет отправитель событий.
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

// Время — всегда RFC3339 в UTC, чтобы файл не зависел от таймзоны того, кто его открыл.
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

// BOM и заголовок пишутся сразу — ранний сбой writer'а виден в NewWriter, не на первой строке.
// Flush после каждой строки: ошибка всплывает на своей строке, не копится до Close.
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
	// Ранний возврат экономит холостой Flush, но не единственная защита — ошибка bufio.Writer
	// внутри encoding/csv «липкая», всплывёт и через Flush()+Error() ниже в любом случае.
	if err := c.cw.Write(row); err != nil {
		return err
	}
	c.cw.Flush()
	return c.cw.Error()
}

// Дублирует Flush()+Error() из Write — рубеж на случай, если промежуточный Flush когда-то уберут.
func (c *csvWriter) Close() error {
	c.cw.Flush()
	return c.cw.Error()
}

// Открывающая скобка — в конструкторе, закрывающая — в Close; на нуле записей выходит «[]».
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

// Без буферизации: json.Encoder пишет прямо в writer при каждом Encode, включая перевод строки.
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
