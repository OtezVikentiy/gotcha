package templates

// экспортируемый дубль ctxRow — дамп собирается в пакете web, где ctxRow недоступен.
type KV struct{ Key, Val string }

// обёртка над неэкспортируемым sentryRequest, переиспользует его parseRequest.
type RequestDump struct {
	Method, URL string
	Query       []KV
	Headers     []KV
	Body        string
}

// nil при пустом или битом JSON.
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
