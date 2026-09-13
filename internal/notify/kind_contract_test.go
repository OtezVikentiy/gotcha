package notify

import "testing"

// Не сверка с другим списком — САМ контракт наружу (versioning.md, alerts.md):
// падает, когда значение константы меняется, а не когда расходится копия.
func TestKindContractValuesFrozen(t *testing.T) {
	pins := []struct {
		name string
		got  string
		want string
	}{
		{"KindNewIssue", KindNewIssue, "new_issue"},
		{"KindRegression", KindRegression, "regression"},
		{"KindSpike", KindSpike, "spike"},
		{"KindSuppressedDigest", KindSuppressedDigest, "suppressed_digest"},
		{"KindNPlusOne", KindNPlusOne, "n_plus_one"},
		{"KindSlowDBQuery", KindSlowDBQuery, "slow_db_query"},
		{"KindHTTPFlood", KindHTTPFlood, "http_flood"},
		{"KindRegressionOpen", KindRegressionOpen, "regression_open"},
		{"KindRegressionClose", KindRegressionClose, "regression_close"},
		{"KindMetricAlertOpen", KindMetricAlertOpen, "metric_alert_open"},
		{"KindMetricAlertResolved", KindMetricAlertResolved, "metric_alert_resolved"},
		{"KindSLOBurnOpen", KindSLOBurnOpen, "slo_burn_open"},
		{"KindSLOBurnClose", KindSLOBurnClose, "slo_burn_close"},
		{"KindProfileRegressionOpen", KindProfileRegressionOpen, "profile_regression_open"},
		{"KindProfileRegressionResolved", KindProfileRegressionResolved, "profile_regression_resolved"},
		{"KindDown", KindDown, "down"},
		{"KindUp", KindUp, "up"},
		{"KindSSLExpiring", KindSSLExpiring, "ssl_expiring"},
		{"KindReminder", KindReminder, "reminder"},
		{"KindHostAlertOpen", KindHostAlertOpen, "host_alert_open"},
		{"KindHostAlertResolved", KindHostAlertResolved, "host_alert_resolved"},
		{"KindHostRetired", KindHostRetired, "host_retired"},
		{"KindChannelTest", KindChannelTest, "channel_test"},
	}
	if len(pins) < 15 {
		t.Fatalf("blind guard: only %d pins — the table itself is broken", len(pins))
	}
	for _, p := range pins {
		if p.got != p.want {
			t.Errorf("notify.%s = %q, want %q — this is a frozen external contract string (versioning.md); changing it is a breaking change", p.name, p.got, p.want)
		}
	}
}
