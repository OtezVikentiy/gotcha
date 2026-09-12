package templates

type SubjectPurgeVM struct {
	Projects []ProjectOption
	// true, когда критерий обезличен при приёме — поиск по нему заведомо пуст.
	InertEmail bool
	InertIP    bool
}

func (v SubjectPurgeVM) InertKey() string {
	switch {
	case v.InertEmail && v.InertIP:
		return "org.gdpr.purge.inert"
	case v.InertEmail:
		return "org.gdpr.purge.inert_email"
	case v.InertIP:
		return "org.gdpr.purge.inert_ip"
	default:
		return ""
	}
}

type ProjectOption struct {
	ID   int64
	Name string
}
