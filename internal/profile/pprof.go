package profile

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	pp "github.com/google/pprof/profile"
)

var ErrProfileTooLarge = errors.New("profile: element count exceeds limit")

const (
	// Своих потолков нет ни у Location/Function/Mapping/string_table — без него
	// декодер материализует их безусловно (см. checkPprofLimits).
	maxProfileLocations = 100000
	maxProfileFunctions = 100000
	maxProfileMappings  = 100000
	maxProfileStrings   = 1000000

	// Провал протобуф-разбора у pp.ParseData падает в legacy ASCII-парсеры
	// (heap profile: ...) без всякого предела; усиление там ~30× на байт.
	maxLegacyPprofBytes = 512 << 10

	// Верхнеуровневые капы не видят packed varint (location_id/value/comment) и
	// вложенные подсообщения (label/line) — их сумму считает maxProfileElements.
	maxProfileElements = 500000

	// Реальная вложенность формата — один уровень (Sample→Label, Location→Line);
	// запас — на случай ошибки в схеме, а не текущего входа.
	maxPprofWalkDepth = 8
)

// errPprofNotClean — форма входа не подтвердилась протобуфом; checkPprofLimits
// превращает его в байтовый потолок неопознанного формата, наружу не течёт.
var errPprofNotClean = errors.New("profile: pprof input did not parse as clean protobuf")

type pprofFieldKind int

const (
	pprofLeaf         pprofFieldKind = iota // скаляр (varint/fixed64/fixed32) или непрозрачные байты/строка
	pprofMessage                            // вложенное подсообщение известной схемы
	pprofPackedVarint                       // packed repeated varint — считать элементы, не подсообщения
)

type pprofFieldSpec struct {
	kind  pprofFieldKind
	child pprofSchema
}

type pprofSchema map[int]pprofFieldSpec

// Function/Mapping/ValueType/Line/Label — в profile.proto без repeated/message
// полей, только скаляры; неизвестный wireType=2 внутри них уходит в pprofLeaf.
var schemaFlat = pprofSchema{}

var schemaLocation = pprofSchema{
	4: {kind: pprofMessage, child: schemaFlat}, // repeated Line line
}

var schemaSample = pprofSchema{
	1: {kind: pprofPackedVarint},               // repeated uint64 location_id
	2: {kind: pprofPackedVarint},               // repeated int64 value
	3: {kind: pprofMessage, child: schemaFlat}, // repeated Label label
}

// checkPprofLimits проверяет форму pprof-протобуфа ДО pp.ParseData. Номера полей
// (sample_type=1..comment=13) сверены со схемой вендора profile.proto.
func checkPprofLimits(data []byte) error {
	budget := maxProfileElements
	if err := walkPprofTop(data, &budget); err != nil {
		if errors.Is(err, errPprofNotClean) {
			return legacyPprofByteCap(len(data))
		}
		return err
	}
	return nil
}

func walkPprofTop(data []byte, budget *int) error {
	var samples, mappings, locations, functions, strings int
	for len(data) > 0 {
		tag, n, ok := readVarint(data)
		if !ok {
			return errPprofNotClean
		}
		data = data[n:]
		field := int(tag >> 3)
		wireType := tag & 7
		switch wireType {
		case 0:
			_, n, ok := readVarint(data)
			if !ok {
				return errPprofNotClean
			}
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return errPprofNotClean
			}
			data = data[8:]
		case 2:
			length, n, ok := readVarint(data)
			if !ok {
				return errPprofNotClean
			}
			data = data[n:]
			if length > uint64(len(data)) {
				return errPprofNotClean
			}
			payload := data[:length]
			data = data[length:]

			switch field {
			// field 1 (sample_type) и прочие нераспознанные поля — просто скип ниже,
			// ValueType дёшев (2 int64) и не рекурсирует, отдельного капа не нужно.
			case 2:
				samples++
				if samples > maxStacks {
					return fmt.Errorf("%w: sample count over %d", ErrProfileTooLarge, maxStacks)
				}
				if err := walkPprofNested(payload, schemaSample, 1, budget); err != nil {
					return err
				}
			case 3:
				mappings++
				if mappings > maxProfileMappings {
					return fmt.Errorf("%w: mapping count over %d", ErrProfileTooLarge, maxProfileMappings)
				}
				if err := walkPprofNested(payload, schemaFlat, 1, budget); err != nil {
					return err
				}
			case 4:
				locations++
				if locations > maxProfileLocations {
					return fmt.Errorf("%w: location count over %d", ErrProfileTooLarge, maxProfileLocations)
				}
				if err := walkPprofNested(payload, schemaLocation, 1, budget); err != nil {
					return err
				}
			case 5:
				functions++
				if functions > maxProfileFunctions {
					return fmt.Errorf("%w: function count over %d", ErrProfileTooLarge, maxProfileFunctions)
				}
				if err := walkPprofNested(payload, schemaFlat, 1, budget); err != nil {
					return err
				}
			case 6:
				strings++
				if strings > maxProfileStrings {
					return fmt.Errorf("%w: string table count over %d", ErrProfileTooLarge, maxProfileStrings)
				}
			case 13: // comment — packed repeated int64, тот же класс, что Sample.location_id/value
				if err := walkPackedVarint(payload, budget); err != nil {
					return err
				}
			}
		case 5:
			if len(data) < 4 {
				return errPprofNotClean
			}
			data = data[4:]
		default:
			// group-теги (3/4) и всё прочее — profile.proto их не использует.
			return errPprofNotClean
		}
	}
	return nil
}

// walkPprofNested тратит общий бюджет на содержимое top-level контейнера; сам
// контейнер в budget не входит — он уже посчитан капом в walkPprofTop.
func walkPprofNested(data []byte, schema pprofSchema, depth int, budget *int) error {
	if depth > maxPprofWalkDepth {
		return errPprofNotClean
	}
	for len(data) > 0 {
		tag, n, ok := readVarint(data)
		if !ok {
			return errPprofNotClean
		}
		data = data[n:]
		field := int(tag >> 3)
		wireType := tag & 7
		switch wireType {
		case 0:
			_, n, ok := readVarint(data)
			if !ok {
				return errPprofNotClean
			}
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return errPprofNotClean
			}
			data = data[8:]
		case 2:
			length, n, ok := readVarint(data)
			if !ok {
				return errPprofNotClean
			}
			data = data[n:]
			if length > uint64(len(data)) {
				return errPprofNotClean
			}
			payload := data[:length]
			data = data[length:]

			spec := schema[field] // отсутствующий field => нулевое значение => pprofLeaf
			switch spec.kind {
			case pprofMessage:
				if *budget <= 0 {
					return fmt.Errorf("%w: nested element budget exhausted", ErrProfileTooLarge)
				}
				*budget--
				if err := walkPprofNested(payload, spec.child, depth+1, budget); err != nil {
					return err
				}
			case pprofPackedVarint:
				if err := walkPackedVarint(payload, budget); err != nil {
					return err
				}
			default:
				// pprofLeaf: скаляры Location/Function/Mapping и нераспознанные поля —
				// настоящий декодер их тоже просто скипает, без аллокации, budget не тратим.
			}
		case 5:
			if len(data) < 4 {
				return errPprofNotClean
			}
			data = data[4:]
		default:
			return errPprofNotClean
		}
	}
	return nil
}

func walkPackedVarint(data []byte, budget *int) error {
	for len(data) > 0 {
		if *budget <= 0 {
			return fmt.Errorf("%w: nested element budget exhausted", ErrProfileTooLarge)
		}
		_, n, ok := readVarint(data)
		if !ok {
			return errPprofNotClean
		}
		data = data[n:]
		*budget--
	}
	return nil
}

func legacyPprofByteCap(total int) error {
	if total > maxLegacyPprofBytes {
		return fmt.Errorf("%w: input not confirmed as protobuf and over %d bytes", ErrProfileTooLarge, maxLegacyPprofBytes)
	}
	return nil
}

func readVarint(data []byte) (v uint64, n int, ok bool) {
	for i := 0; i < len(data) && i < 10; i++ {
		b := data[i]
		v |= uint64(b&0x7f) << uint(7*i)
		if b&0x80 == 0 {
			return v, i + 1, true
		}
	}
	return 0, 0, false
}

func ParsePprof(raw []byte, sampleType string, now time.Time) (Profile, error) {
	if err := checkPprofLimits(raw); err != nil {
		return Profile{}, err
	}
	p, err := pp.ParseData(raw)
	if err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrBadProfile, err)
	}
	if len(p.SampleType) == 0 {
		return Profile{}, fmt.Errorf("%w: no sample types", ErrBadProfile)
	}
	idx := len(p.SampleType) - 1
	if sampleType != "" {
		for i, st := range p.SampleType {
			if st.Type == sampleType {
				idx = i
				break
			}
		}
	}
	typ := p.SampleType[idx].Type
	unit := p.SampleType[idx].Unit

	budget := maxStackBytes
	var samples []Sample
	var truncReason string
	for _, s := range p.Sample {
		if idx >= len(s.Value) {
			continue
		}
		v := s.Value[idx]
		if v <= 0 {
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
		stack := make([]Frame, 0, min(len(s.Location), maxFrames))
	frames:
		for i := len(s.Location) - 1; i >= 0; i-- {
			loc := s.Location[i]
			for j := len(loc.Line) - 1; j >= 0; j-- {
				if len(stack) >= maxFrames {
					if truncReason == "" {
						truncReason = "frame_count"
					}
					break frames
				}
				if budget <= 0 {
					if truncReason == "" {
						truncReason = "stack_byte_budget"
					}
					break frames
				}
				ln := loc.Line[j]
				fn := ln.Function
				if fn == nil {
					continue
				}
				name := capRunes(fn.Name, maxFrameField)
				file := capRunes(fn.Filename, maxFrameField)
				// как в ParseSentry: бюджет списывается по факту байт имён, а не по
				// счётным капам, которые перемножаются на вложенных Location×Line.
				budget -= len(name) + len(file) + frameOverheadBytes
				stack = append(stack, Frame{
					Function: name,
					File:     file,
					Line:     int32(ln.Line),
				})
			}
		}
		if len(stack) == 0 {
			continue
		}
		samples = append(samples, Sample{Stack: stack, Value: uint64(v)})
	}
	if truncReason != "" {
		slog.Warn("profile truncated on accept", "parser", "pprof", "reason", truncReason)
	}

	return Profile{Type: typ, Unit: unit, Timestamp: now, Samples: samples, Truncated: truncReason != ""}, nil
}
