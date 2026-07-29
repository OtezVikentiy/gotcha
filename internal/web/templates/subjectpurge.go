package templates

// SubjectPurgeVM — состояние блока «удаление данных субъекта» (152-ФЗ ст. 14).
//
// Существует потому, что операция умела завершаться «успешно», не тронув ни
// одной строки, и оператор этого не видел: редирект выглядел одинаково и после
// удаления 128 записей, и после нуля. Случай не гипотетический, а поведение по
// умолчанию — GOTCHA_SCRUB_IP и GOTCHA_SCRUB_EMAIL включены, значит колонки
// user_email и user_ip зануляются на приёме и поиск по ним не совпадает ни с
// чем. Работает только user_id: он намеренно исключён из скрубинга ровно ради
// этого права субъекта.
type SubjectPurgeVM struct {
	// Done — удаление только что выполнялось (страница открыта после редиректа).
	Done bool
	// Purged — сколько строк отнесено к субъекту и удалено.
	Purged int
	// InertEmail/InertIP — на этом инстансе соответствующий критерий заведомо
	// пуст, потому что значение обезличивается при приёме.
	InertEmail bool
	InertIP    bool
}

// InertKey возвращает ключ предупреждения о заведомо пустых критериях поиска
// или "" , если оба критерия рабочие.
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
