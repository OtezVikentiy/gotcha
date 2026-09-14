package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

var ErrBadProfile = errors.New("profile: malformed profile")

const (
	maxFrames    = 1024
	maxStacks    = 100000
	maxMetaField = 200
	// формат индексный (Stacks ссылаются на Frames по индексу): без кэпа на разборе
	// одно огромное имя, упомянутое maxFrames раз, раздувается при склейке ключа в Writer.Add.
	maxFrameField = 512
	// бюджет на СУММУ байт развёрнутых кадров: счётные капы (maxFrames×maxStacks) перемножаются
	// и позволяют гигабайты аллокаций из КБ входа — байтовый предел режет независимо от формы стеков.
	maxStackBytes = 16 << 20
	// нужен, иначе кадр с пустыми function/filename не тратит бюджет вовсе, и защита
	// откатывается к прежним счётным капам.
	frameOverheadBytes = 64
)

// дубль ingest.capRunes: profile не должен зависеть от приёмного слоя.
func capRunes(s string, n int) string {
	// len(s) <= n гарантирует рун <= n (UTF-8: байт всегда >= рун) — без []rune-копии.
	if len(s) <= n {
		return s
	}
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

// Service у Sentry-профиля нет — оставляем пустым, не пропущенное поле.
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

	counts := make(map[int]uint64)
	for _, s := range it.Profile.Samples {
		counts[s.StackID]++
	}

	// кап кадров — здесь, один раз до разворачивания стеков: формат индексный,
	// один и тот же кадр иначе капался бы на каждом упоминании в стеках.
	frames := make([]Frame, len(it.Profile.Frames))
	for i, fr := range it.Profile.Frames {
		frames[i] = Frame{
			Function: capRunes(fr.Function, maxFrameField),
			File:     capRunes(fr.Filename, maxFrameField),
			Line:     fr.Lineno,
		}
	}

	// сортировка по value, затем stackID: обход map случаен, а бюджет обрежет
	// список — без детерминированного порядка один и тот же POST давал бы разный результат.
	ordered := make([]int, 0, len(counts))
	for stackID := range counts {
		ordered = append(ordered, stackID)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		return a < b
	})

	budget := maxStackBytes
	var samples []Sample
	var truncReason string
	for _, stackID := range ordered {
		value := counts[stackID]
		if stackID < 0 || stackID >= len(it.Profile.Stacks) {
			continue
		}
		if len(samples) >= maxStacks {
			if truncReason == "" {
				truncReason = "sample_count"
			}
			break
		}
		if budget <= 0 {
			if truncReason == "" {
				truncReason = "stack_byte_budget"
			}
			break
		}
		idxs := it.Profile.Stacks[stackID]
		// ёмкость по min(len, maxFrames) — иначе преаллокация тратит память по
		// недоверенной длине до того, как сработает кап числа кадров.
		stack := make([]Frame, 0, min(len(idxs), maxFrames))
		for i := len(idxs) - 1; i >= 0; i-- {
			if len(stack) >= maxFrames {
				if truncReason == "" {
					truncReason = "frame_count"
				}
				break
			}
			if budget <= 0 {
				if truncReason == "" {
					truncReason = "stack_byte_budget"
				}
				break
			}
			fi := idxs[i]
			if fi < 0 || fi >= len(frames) {
				continue
			}
			fr := frames[fi]
			// кадры уже каппированы выше, здесь без копий; бюджет списывается по
			// факту байт имён — они станут ключом стека.
			budget -= len(fr.Function) + len(fr.File) + frameOverheadBytes
			stack = append(stack, fr)
		}
		if len(stack) == 0 {
			continue
		}
		samples = append(samples, Sample{Stack: stack, Value: value})
	}
	if truncReason != "" {
		slog.Warn("profile truncated on accept", "parser", "sentry", "reason", truncReason)
	}

	return Profile{
		Environment: capRunes(it.Environment, maxMetaField),
		Transaction: capRunes(transaction, maxMetaField),
		Platform:    capRunes(it.Platform, maxMetaField),
		Type:        "cpu",
		// число сэмплов с этим стеком, не время — единица «count», не наносекунды.
		Unit:      "count",
		TraceID:   capRunes(traceID, maxMetaField),
		Timestamp: now,
		Samples:   samples,
		Truncated: truncReason != "",
	}, nil
}
