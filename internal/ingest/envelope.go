// Package ingest принимает события по Sentry-протоколу: envelope/store
// эндпойнты, DSN-аутентификация, парсинг и конвейер обработки.
package ingest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrTooLarge — item в envelope превышает лимит размера.
var ErrTooLarge = errors.New("ingest: envelope item too large")

// Envelope — распакованный envelope: заголовок и payload'ы item'ов, которые мы
// умеем обрабатывать: события (type=event) и транзакции (type=transaction).
type Envelope struct {
	EventID string
	Events  [][]byte
	// Transactions — payload'ы item'ов type=transaction; идут отдельным путём
	// (свой парсер, своя квота, своё семплирование), см. Handler.envelope.
	Transactions [][]byte
}

// ParseEnvelope разбирает envelope-формат Sentry: JSON-заголовок, затем
// пары (JSON-заголовок item'а, payload). Item'ы прочих типов (session,
// attachment, client_report...) пропускаются.
func ParseEnvelope(r io.Reader, maxItem int64) (*Envelope, error) {
	br := bufio.NewReader(r)

	headerLine, err := readLine(br, maxItem)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("ingest: envelope header: %w", err)
	}
	if len(headerLine) == 0 {
		return nil, fmt.Errorf("ingest: envelope header: empty envelope")
	}
	var header struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(headerLine, &header); err != nil {
		return nil, fmt.Errorf("ingest: envelope header: %w", err)
	}
	env := &Envelope{EventID: header.EventID}

	for {
		itemLine, err := readLine(br, maxItem)
		if errors.Is(err, io.EOF) && len(itemLine) == 0 {
			return env, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("ingest: item header: %w", err)
		}
		if len(itemLine) == 0 {
			return env, nil
		}
		var ih struct {
			Type   string `json:"type"`
			Length *int64 `json:"length"`
		}
		if err := json.Unmarshal(itemLine, &ih); err != nil {
			return nil, fmt.Errorf("ingest: item header: %w", err)
		}

		var payload []byte
		if ih.Length != nil {
			if *ih.Length < 0 {
				return nil, fmt.Errorf("ingest: item payload: malformed negative length %d", *ih.Length)
			}
			if *ih.Length > maxItem {
				return nil, ErrTooLarge
			}
			payload = make([]byte, *ih.Length)
			if _, err := io.ReadFull(br, payload); err != nil {
				return nil, fmt.Errorf("ingest: item payload: %w", err)
			}
			// Съесть перевод строки после payload'а (может отсутствовать в конце).
			if b, err := br.ReadByte(); err == nil && b != '\n' {
				_ = br.UnreadByte()
			}
		} else {
			payload, err = readLine(br, maxItem)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("ingest: item payload: %w", err)
			}
		}

		switch ih.Type {
		case "event":
			env.Events = append(env.Events, payload)
		case "transaction":
			env.Transactions = append(env.Transactions, payload)
		}
	}
}

// readLine читает строку без завершающего \n, не давая ей вырасти
// больше limit байт (защита от memory exhaustion на untrusted входе).
func readLine(br *bufio.Reader, limit int64) ([]byte, error) {
	var buf []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return buf, err
		}
		if b == '\n' {
			return buf, nil
		}
		buf = append(buf, b)
		if int64(len(buf)) > limit {
			return nil, ErrTooLarge
		}
	}
}
