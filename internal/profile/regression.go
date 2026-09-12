package profile

type RegressionConfig struct {
	ThresholdPct float64
	// должно быть меньше ThresholdPct, иначе гистерезис не защищает от дребезга.
	RecoveryPct   float64
	WindowMinutes int
	BaselineDays  int
	MinSamples    int
	ShareFloor    float64
	TopK          int
}

func DefaultProfileRegressionConfig() RegressionConfig {
	return RegressionConfig{
		ThresholdPct:  0.5,
		RecoveryPct:   0.2,
		WindowMinutes: 60,
		BaselineDays:  7,
		MinSamples:    100,
		ShareFloor:    0.05,
		TopK:          20,
	}
}

type DecisionKind int

const (
	DecisionNone DecisionKind = iota
	DecisionOpen
	DecisionResolve
	DecisionBump
)

type Decision struct {
	Kind DecisionKind
}

func Decide(baseShare, recentShare float64, baseSamples, recentSamples uint64, cfg RegressionConfig, open bool) Decision {
	if recentSamples < uint64(cfg.MinSamples) {
		return Decision{Kind: DecisionNone}
	}
	if open {
		// без ветки ShareFloor инцидент с усохшей до нуля базой навсегда застревал бы в Bump.
		if baseShare > 0 {
			if recentShare <= baseShare*(1+cfg.RecoveryPct) {
				return Decision{Kind: DecisionResolve}
			}
			return Decision{Kind: DecisionBump}
		}
		if recentShare <= cfg.ShareFloor {
			return Decision{Kind: DecisionResolve}
		}
		return Decision{Kind: DecisionBump}
	}
	if baseSamples < uint64(cfg.MinSamples) {
		return Decision{Kind: DecisionNone}
	}
	if recentShare >= cfg.ShareFloor && baseShare > 0 && recentShare > baseShare*(1+cfg.ThresholdPct) {
		return Decision{Kind: DecisionOpen}
	}
	return Decision{Kind: DecisionNone}
}
