package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestYandexExchange(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "token_type": "bearer"})
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "OAuth at-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "42", "default_email": "u@yandex.ru", "display_name": "U",
		})
	})

	p := NewYandex(YandexConfig{ClientID: "cid", ClientSecret: "sec"})
	p.tokenURL = srv.URL + "/token"
	p.infoURL = srv.URL + "/info"

	au := p.AuthURL("S", "", "", "https://gotcha/cb")
	u, _ := url.Parse(au)
	if u.Query().Get("state") != "S" || u.Query().Get("client_id") != "cid" || u.Query().Get("response_type") != "code" {
		t.Fatalf("AuthURL wrong: %s", au)
	}

	id, err := p.Exchange(context.Background(), "code", "", "https://gotcha/cb", "")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "42" || id.Email != "u@yandex.ru" || !id.EmailVerified || id.DisplayName != "U" {
		t.Fatalf("Identity = %+v", id)
	}
}

func TestYandexExchangeNoEmail(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at"})
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "7"}) // без default_email
	})
	p := NewYandex(YandexConfig{ClientID: "cid", ClientSecret: "sec"})
	p.tokenURL = srv.URL + "/token"
	p.infoURL = srv.URL + "/info"
	if _, err := p.Exchange(context.Background(), "c", "", "cb", ""); err == nil {
		t.Fatal("no email must fail")
	}
}
