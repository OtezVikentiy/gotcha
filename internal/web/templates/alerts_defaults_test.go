package templates

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
)

// Дефолты несохранённого spike-правила обязаны совпадать с internal/alert —
// иначе форма создания предложит числа, которых сервис не подтвердит как свои.
func TestRuleByKindSpikeDefaultsMatchAlertPackage(t *testing.T) {
	r := ruleByKind(nil, alert.KindSpike)
	if r.Threshold != alert.DefaultSpikeThreshold {
		t.Errorf("Threshold = %d, want alert.DefaultSpikeThreshold (%d)", r.Threshold, alert.DefaultSpikeThreshold)
	}
	if r.WindowMinutes != alert.DefaultSpikeWindowMinutes {
		t.Errorf("WindowMinutes = %d, want alert.DefaultSpikeWindowMinutes (%d)", r.WindowMinutes, alert.DefaultSpikeWindowMinutes)
	}
	if r.ThrottleMinutes != alert.DefaultThrottleMinutes {
		t.Errorf("ThrottleMinutes = %d, want alert.DefaultThrottleMinutes (%d)", r.ThrottleMinutes, alert.DefaultThrottleMinutes)
	}
}

func TestRuleByKindOtherThrottleMatchesAlertPackage(t *testing.T) {
	r := ruleByKind(nil, alert.KindNewIssue)
	if r.ThrottleMinutes != alert.DefaultThrottleMinutes {
		t.Errorf("ThrottleMinutes = %d, want alert.DefaultThrottleMinutes (%d)", r.ThrottleMinutes, alert.DefaultThrottleMinutes)
	}
}
