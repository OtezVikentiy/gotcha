package notify

// Единственное место, где рождается строка вида вебхука — остальной код
// ссылается на эти константы. Значения заморожены контрактом, пин — TestKindContractValuesFrozen.
const (
	KindNewIssue   = "new_issue"
	KindRegression = "regression"
	KindSpike      = "spike"

	KindSuppressedDigest = "suppressed_digest"

	KindNPlusOne    = "n_plus_one"
	KindSlowDBQuery = "slow_db_query"
	KindHTTPFlood   = "http_flood"

	KindRegressionOpen  = "regression_open"
	KindRegressionClose = "regression_close"

	KindMetricAlertOpen     = "metric_alert_open"
	KindMetricAlertResolved = "metric_alert_resolved"

	KindSLOBurnOpen  = "slo_burn_open"
	KindSLOBurnClose = "slo_burn_close"

	KindProfileRegressionOpen     = "profile_regression_open"
	KindProfileRegressionResolved = "profile_regression_resolved"

	KindDown        = "down"
	KindUp          = "up"
	KindSSLExpiring = "ssl_expiring"
	KindReminder    = "reminder"

	KindHostAlertOpen     = "host_alert_open"
	KindHostAlertResolved = "host_alert_resolved"
	KindHostRetired       = "host_retired"

	// Мимо redactedKindKeys намеренно — тестовая кнопка канала шлёт эту кинду
	// напрямую (internal/web/alerts.go), приватность к ней не применяется.
	KindChannelTest = "channel_test"
)
