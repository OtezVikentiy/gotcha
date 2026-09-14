package ingest

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/scrub"
)

func TestScrubProfileMetadata(t *testing.T) {
	h := &Handler{Scrub: scrub.NewScrubber(true, true, []string{"token"})}

	p := profile.Profile{
		Transaction: "GET https://api.example.com/users?token=S3CRET&id=7",
		Service:     "https://svc/?token=S3CRET",
		Environment: "prod",
	}
	h.scrubProfile(&p)

	if strings.Contains(p.Transaction, "S3CRET") {
		t.Errorf("токен пережил скрубинг имени транзакции: %q", p.Transaction)
	}
	if strings.Contains(p.Service, "S3CRET") {
		t.Errorf("токен пережил скрубинг сервиса: %q", p.Service)
	}
	if p.Environment != "prod" {
		t.Errorf("environment изменён напрасно: %q", p.Environment)
	}

	h2 := &Handler{}
	p2 := profile.Profile{Transaction: "x?token=S3CRET"}
	h2.scrubProfile(&p2)
	if p2.Transaction != "x?token=S3CRET" {
		t.Errorf("без скрубера значение изменено: %q", p2.Transaction)
	}
}
