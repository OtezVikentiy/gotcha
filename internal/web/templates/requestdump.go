package templates

// KV — пара ключ/значение для машинной сборки (LLM-дамп события). Экспортируемый
// аналог неэкспортируемого ctxRow, чтобы дамп собирался в пакете web.
type KV struct{ Key, Val string }

// RequestDump — HTTP-запрос события в форме, пригодной для текстового дампа.
// Экспортируемая обёртка над sentryRequest (неэкспортируем): переиспользует тот же
// parseRequest, что и рендер страницы, без дублирования разбора JSON.
type RequestDump struct {
	Method, URL string
	Query       []KV
	Headers     []KV
	Body        string
}

// RequestForDump разбирает Sentry request-интерфейс через существующий
// parseRequest и отдаёт экспортируемую структуру. nil — пусто/битый JSON.
func RequestForDump(requestJSON string) *RequestDump {
	r := parseRequest(requestJSON)
	if r == nil {
		return nil
	}
	return &RequestDump{
		Method:  r.Method,
		URL:     r.URL,
		Query:   toKVs(r.Query),
		Headers: toKVs(r.Headers),
		Body:    r.Body,
	}
}

func toKVs(rows []ctxRow) []KV {
	if len(rows) == 0 {
		return nil
	}
	out := make([]KV, len(rows))
	for i, r := range rows {
		out[i] = KV{Key: r.Key, Val: r.Val}
	}
	return out
}
