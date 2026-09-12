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

	"gitflic.ru/otezvikentiy/gotcha/internal/netguard"
)

const httpTimeout = 5 * time.Second

var sharedClient = netguard.SafeHTTPClient(false, httpTimeout)

func SetAllowPrivateHosts(allow bool) {
	sharedClient = netguard.SafeHTTPClient(allow, httpTimeout)
}

func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func PKCE() (verifier, challenge string, err error) {
	verifier, err = RandomToken()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

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

func decodeJSON(r io.Reader, dst any) error {
	return json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(dst)
}

func nowUnix() int64 { return time.Now().Unix() }

const clockSkewLeeway int64 = 60
