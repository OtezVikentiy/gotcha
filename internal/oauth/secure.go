package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpTimeout — потолок на любой вызов провайдера. Без ретраев в горячем пути.
const httpTimeout = 5 * time.Second

// sharedClient переиспользуется всеми адаптерами (пул соединений).
var sharedClient = &http.Client{Timeout: httpTimeout}

// RandomToken — 32 случайных байта в base64url (state, nonce).
func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// PKCE возвращает verifier и S256-challenge (RFC 7636).
func PKCE() (verifier, challenge string, err error) {
	verifier, err = RandomToken()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// getJSON — GET c декодом JSON-тела (лимит 1 MiB), таймаут общего клиента.
func getJSON(ctx context.Context, rawURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := sharedClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
	return decodeJSON(resp.Body, dst)
}

// postForm — POST application/x-www-form-urlencoded с декодом JSON-ответа.
func postForm(ctx context.Context, rawURL string, form url.Values, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := sharedClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: status %d", rawURL, resp.StatusCode)
	}
	return decodeJSON(resp.Body, dst)
}

// decodeJSON декодирует тело (лимит 1 MiB) в dst.
func decodeJSON(r io.Reader, dst any) error {
	return json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(dst)
}

// nowUnix — текущее время в секундах (обёртка ради тестируемости exp).
func nowUnix() int64 { return time.Now().Unix() }
