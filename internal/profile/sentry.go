package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrBadProfile = errors.New("profile: malformed profile")

const (
	maxFrames = 1024
	maxStacks = 100000
	// maxMetaField — кап недоверенных строковых полей профиля (environment/
	// transaction/platform/trace_id) до записи, как capRunes в пакете ingest
	// (otlp.go/transaction.go — те же 200 рун). Свой хелпер здесь, а не импорт
	// из ingest: пакет profile не должен зависеть от приёмного слоя.
	maxMetaField = 200
)

// capRunes обрезает недоверенную строку профиля до n рун (дубль ingest.capRunes:
// поля профиля из SDK не должны раздувать колонки/индексы БД без ограничений).
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type sentryEnvelopeItem struct {
	Platform    string `json:"platform"`
	Environment string `json:"environment"`
	Release     string `json:"release"`
	Transaction struct {
		Name    string `json:"name"`
		TraceID string `json:"trace_id"`
	} `json:"transaction"`
	Transactions []struct {
		Name    string `json:"name"`
		TraceID string `json:"trace_id"`
	} `json:"transactions"`
	Profile struct {
		Frames []struct {
			Function string `json:"function"`
			Filename string `json:"filename"`
			Lineno   int32  `json:"lineno"`
		} `json:"frames"`
		Stacks  [][]int `json:"stacks"`
		Samples []struct {
			StackID int `json:"stack_id"`
		} `json:"samples"`
	} `json:"profile"`
}

// ParseSentry разбирает Sentry profile sample-format v1 в общую модель. Сэмплы
// группируются по stack_id (value=count), стеки переворачиваются из лист→корень
// в корень→лист. Service у Sentry-профиля нет — оставляем пустым.
func ParseSentry(raw []byte, now time.Time) (Profile, error) {
	var it sentryEnvelopeItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrBadProfile, err)
	}
	transaction := it.Transaction.Name
	traceID := it.Transaction.TraceID
	if len(it.Transactions) > 0 {
		if transaction == "" {
			transaction = it.Transactions[0].Name
		}
		if traceID == "" {
			traceID = it.Transactions[0].TraceID
		}
	}

	// value по stack_id.
	counts := make(map[int]uint64)
	for _, s := range it.Profile.Samples {
		counts[s.StackID]++
	}

	frames := it.Profile.Frames
	var samples []Sample
	for stackID, value := range counts {
		if stackID < 0 || stackID >= len(it.Profile.Stacks) {
			continue // неизвестный стек — пропуск
		}
		if len(samples) >= maxStacks {
			break
		}
		idxs := it.Profile.Stacks[stackID]
		// Переворот лист→корень → корень→лист + кап кадров.
		stack := make([]Frame, 0, len(idxs))
		for i := len(idxs) - 1; i >= 0; i-- {
			if len(stack) >= maxFrames {
				break
			}
			fi := idxs[i]
			if fi < 0 || fi >= len(frames) {
				continue
			}
			fr := frames[fi]
			stack = append(stack, Frame{Function: fr.Function, File: fr.Filename, Line: fr.Lineno})
		}
		if len(stack) == 0 {
			continue
		}
		samples = append(samples, Sample{Stack: stack, Value: value})
	}

	return Profile{
		// Недоверенные строковые поля из payload'а каппим до записи (maxMetaField),
		// как и остальные строки приёма.
		Environment: capRunes(it.Environment, maxMetaField),
		Transaction: capRunes(transaction, maxMetaField),
		Platform:    capRunes(it.Platform, maxMetaField),
		Type: "cpu",
		// В формате Sentry значение выборки — число сэмплов с этим стеком
		// (см. counts выше), а не время: единица «count», а не наносекунды.
		Unit:      "count",
		TraceID:   capRunes(traceID, maxMetaField),
		Timestamp:   now,
		Samples:     samples,
	}, nil
}
