package ingest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var ErrTooLarge = errors.New("ingest: envelope item too large")

// Квота списывается на HTTP-запрос, а не на item — без предела envelope с
// тысячами item'ов давал бы неограниченную амплификацию. Согласован с maxSpans=1000.
const maxEnvelopeItems = 1000

type Envelope struct {
	EventID string
	Events  [][]byte
	// Свой парсер, своя квота, своё семплирование — см. Handler.envelope.
	Transactions [][]byte
	// Свой парсер (profile.ParseSentry) и своя квота — см. Handler.envelope.
	Profiles [][]byte
	Dropped  int
	// Поштучный отбор, не «весь envelope в отказ»: item'ы независимы, событие не
	// должно теряться из-за соседнего profile-item'а. nil/пустая карта — ничего не отброшено.
	ScopeRejected map[IngestSignal]int
}

// allow=nil — всё разрешено (разбор без скоупа, как в тестах формата).
func ParseEnvelope(r io.Reader, maxItem int64, allow func(IngestSignal) bool) (*Envelope, error) {
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

		// Считаем только известные типы — прочие не создают амплификацию. Сверх предела
		// payload уже прочитан из потока (иначе не сдвинуть reader), но не сохраняется.
		switch ih.Type {
		case "event", "transaction", "profile":
			known++
			if known > maxEnvelopeItems {
				env.Dropped++
				continue
			}
			signal := envelopeItemSignal(ih.Type)
			if allow != nil && !allow(signal) {
				if env.ScopeRejected == nil {
					env.ScopeRejected = map[IngestSignal]int{}
				}
				env.ScopeRejected[signal]++
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

// Соответствие однозначно: других известных типов в switch выше нет, а
// неизвестные до сюда не доходят.
func envelopeItemSignal(itemType string) IngestSignal {
	switch itemType {
	case "transaction":
		return SignalTransaction
	case "profile":
		return SignalProfile
	default:
		return SignalEvent
	}
}

// Без завершающего \n; лимит — защита от memory exhaustion на untrusted входе.
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
