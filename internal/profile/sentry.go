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
)

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
		Environment: it.Environment,
		Transaction: transaction,
		Platform:    it.Platform,
		Type: "cpu",
		// В формате Sentry значение выборки — число сэмплов с этим стеком
		// (см. counts выше), а не время: единица «count», а не наносекунды.
		Unit:      "count",
		TraceID:   traceID,
		Timestamp:   now,
		Samples:     samples,
	}, nil
}
