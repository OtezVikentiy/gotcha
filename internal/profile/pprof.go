package profile

import (
	"fmt"
	"time"

	pp "github.com/google/pprof/profile"
)

// ParsePprof разбирает pprof (gzip-protobuf) в общую модель. sampleType — имя
// желаемого типа значений (напр. "cpu"/"samples"); если пусто или не найдено —
// берётся последний тип. Кадры pprof лист→корень переворачиваются в корень→лист.
func ParsePprof(raw []byte, sampleType string, now time.Time) (Profile, error) {
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

	var samples []Sample
	for _, s := range p.Sample {
		if idx >= len(s.Value) {
			continue
		}
		v := s.Value[idx]
		if v <= 0 {
			continue
		}
		// Location лист→корень → переворот в корень→лист; у Location может быть
		// несколько Line (inlining), берём их в обратном порядке для того же
		// направления корень→лист.
		stack := make([]Frame, 0, len(s.Location))
		for i := len(s.Location) - 1; i >= 0; i-- {
			loc := s.Location[i]
			for j := len(loc.Line) - 1; j >= 0; j-- {
				if len(stack) >= maxFrames {
					break
				}
				ln := loc.Line[j]
				fn := ln.Function
				if fn == nil {
					continue
				}
				stack = append(stack, Frame{Function: fn.Name, File: fn.Filename, Line: int32(ln.Line)})
			}
		}
		if len(stack) == 0 {
			continue
		}
		if len(samples) >= maxStacks {
			break
		}
		samples = append(samples, Sample{Stack: stack, Value: uint64(v)})
	}

	return Profile{Type: typ, Timestamp: now, Samples: samples}, nil
}
