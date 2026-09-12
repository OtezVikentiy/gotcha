package uptime_test

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestAggregateUnknownWhenNoDecidedRegions(t *testing.T) {
	m := uptime.Monitor{Consensus: uptime.ConsensusMajority}
	if got := uptime.Aggregate(m, nil); got != "unknown" {
		t.Fatalf("Aggregate(no states) = %q, want unknown", got)
	}
	states := []uptime.State{{Region: "r1", Status: "unknown"}}
	if got := uptime.Aggregate(m, states); got != "unknown" {
		t.Fatalf("Aggregate(all unknown) = %q, want unknown", got)
	}
}

func TestAggregateMatchesConsensusPolicy(t *testing.T) {
	cases := []struct {
		name      string
		consensus uptime.Consensus
		statuses  []string
		want      string
	}{
		{"any: one down", uptime.ConsensusAny, []string{"up", "down", "up"}, "down"},
		{"any: all up", uptime.ConsensusAny, []string{"up", "up"}, "up"},
		{"majority: one of three down is not enough", uptime.ConsensusMajority, []string{"down", "up", "up"}, "up"},
		{"majority: two of three down", uptime.ConsensusMajority, []string{"down", "down", "up"}, "down"},
		{"all: not all down", uptime.ConsensusAll, []string{"down", "up"}, "up"},
		{"all: all down", uptime.ConsensusAll, []string{"down", "down"}, "down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := uptime.Monitor{Consensus: tc.consensus}
			states := make([]uptime.State, len(tc.statuses))
			for i, st := range tc.statuses {
				states[i] = uptime.State{Region: "r", Status: st}
			}
			if got := uptime.Aggregate(m, states); got != tc.want {
				t.Fatalf("Aggregate(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
