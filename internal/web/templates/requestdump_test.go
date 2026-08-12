package templates

import "testing"

func TestRequestForDump(t *testing.T) {
	const raw = `{"method":"POST","url":"https://app/x",` +
		`"query_string":"a=1&b=2","headers":{"Content-Type":"application/json"},` +
		`"data":"{\"id\":7}"}`
	d := RequestForDump(raw)
	if d == nil {
		t.Fatal("RequestForDump = nil, want parsed")
	}
	if d.Method != "POST" || d.URL != "https://app/x" {
		t.Errorf("method/url = %q %q", d.Method, d.URL)
	}
	if len(d.Query) != 2 || d.Query[0].Key != "a" {
		t.Errorf("query = %+v", d.Query)
	}
	if len(d.Headers) != 1 || d.Headers[0].Key != "Content-Type" {
		t.Errorf("headers = %+v", d.Headers)
	}
	if d.Body == "" {
		t.Errorf("body empty")
	}
	if RequestForDump("") != nil || RequestForDump("{}") != nil {
		t.Errorf("empty JSON must map to nil")
	}
}
