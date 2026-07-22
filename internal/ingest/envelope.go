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

// maxEnvelopeItems — верхняя граница числа обрабатываемых item'ов (event+
// transaction+profile) в ОДНОМ envelope'е. Квота списывается раз на HTTP-запрос,
// а не на item (см. QuotaChecker), поэтому без этого предела один envelope с
// сотнями тысяч крошечных item'ов даёт неограниченную амплификацию: одна
// единица квоты → сотни тысяч enqueue/upsert'ов в PG/CH. Тело уже ограничено по
// БАЙТАМ (Handler.body), это — ограничение по ШТУКАМ. Согласован с maxSpans=1000.
// Лишние item'ы отбрасываются (учитываются в Envelope.Dropped), уже разобранные
// принимаются — как maxSpans роняет лишние спаны, но оставляет транзакцию.
const maxEnvelopeItems = 1000

// Envelope — распакованный envelope: заголовок и payload'ы item'ов, которые мы
// умеем обрабатывать: события (type=event) и транзакции (type=transaction).
type Envelope struct {
	EventID string
	Events  [][]byte
	// Transactions — payload'ы item'ов type=transaction; идут отдельным путём
	// (свой парсер, своя квота, своё семплирование), см. Handler.envelope.
	Transactions [][]byte
	// Profiles — payload'ы item'ов type=profile (этап 7); свой парсер
	// (profile.ParseSentry) и своя квота, см. Handler.envelope.
	Profiles [][]byte
	// Dropped — число известных item'ов (event/transaction/profile),
	// отброшенных по лимиту maxEnvelopeItems. Handler считает их дропом
	// (best-effort) и логирует; 0 — предел не достигнут.
	Dropped int
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

	known := 0 // известные item'ы (event/transaction/profile) — для maxEnvelopeItems
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

		// Считаем и каппим только ИЗВЕСТНЫЕ типы: прочие (session/attachment/
		// client_report) и так игнорируются и амплификацию не создают. Сверх
		// предела payload уже прочитан из потока (иначе не сдвинуть reader), но
		// НЕ сохраняется — downstream-работа (enqueue/upsert) ограничена.
		switch ih.Type {
		case "event", "transaction", "profile":
			known++
			if known > maxEnvelopeItems {
				env.Dropped++
				continue
			}
			switch ih.Type {
			case "event":
				env.Events = append(env.Events, payload)
			case "transaction":
				env.Transactions = append(env.Transactions, payload)
			case "profile":
				env.Profiles = append(env.Profiles, payload)
			}
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
