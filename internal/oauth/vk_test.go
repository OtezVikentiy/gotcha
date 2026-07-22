package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestVKExchange(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		// VK отдаёт user_id и email рядом с access_token.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "user_id": 777, "email": "u@vk.com",
		})
	})
	mux.HandleFunc("/users.get", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": []map[string]any{{"id": 777, "first_name": "Ivan", "last_name": "Petrov"}},
		})
	})
	p := NewVK(VKConfig{ClientID: "cid", ClientSecret: "sec"})
	p.tokenURL = srv.URL + "/token"
	p.usersURL = srv.URL + "/users.get"

	au := p.AuthURL("S", "", "", "https://gotcha/cb")
	u, _ := url.Parse(au)
	if u.Query().Get("scope") != "email" || u.Query().Get("client_id") != "cid" {
		t.Fatalf("AuthURL wrong: %s", au)
	}

	id, err := p.Exchange(context.Background(), "code", "", "https://gotcha/cb", "")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "777" || id.Email != "u@vk.com" || !id.EmailVerified || !id.TrustedIssuer || id.DisplayName != "Ivan Petrov" {
		t.Fatalf("Identity = %+v", id)
	}
}

func TestVKExchangeNoEmail(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "user_id": 1}) // без email
	})
	p := NewVK(VKConfig{ClientID: "cid", ClientSecret: "sec"})
	p.tokenURL = srv.URL + "/token"
	p.usersURL = srv.URL + "/users.get"
	if _, err := p.Exchange(context.Background(), "c", "", "cb", ""); err == nil {
		t.Fatal("no email must fail")
	}
}
