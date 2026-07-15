package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

const (
	oauthCookieName = "gotcha_oauth"
	oauthFlowTTL    = 600 // секунд: короткоживущая cookie на время редиректа к провайдеру
)

var (
	errFlowMalformed = errors.New("web: malformed oauth flow cookie")
	errFlowBadSig    = errors.New("web: oauth flow signature mismatch")
	errFlowExpired   = errors.New("web: oauth flow expired")
)

// oauthFlow — состояние потока, живущее в подписанной cookie между start и
// callback. Link/UID — поток привязки из профиля (юзер уже залогинен).
type oauthFlow struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Link     bool   `json:"l"`
	UID      int64  `json:"u"`
	IssuedAt int64  `json:"t"`
}

func signFlow(secret []byte, f oauthFlow) (string, error) {
	payload, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return body + "." + sig, nil
}

func parseFlow(secret []byte, raw string, nowUnix int64) (oauthFlow, error) {
	body, sig, ok := strings.Cut(raw, ".")
	if !ok {
		return oauthFlow{}, errFlowMalformed
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return oauthFlow{}, errFlowBadSig
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return oauthFlow{}, errFlowMalformed
	}
	var f oauthFlow
	if err := json.Unmarshal(payload, &f); err != nil {
		return oauthFlow{}, errFlowMalformed
	}
	if nowUnix-f.IssuedAt > oauthFlowTTL || nowUnix < f.IssuedAt {
		return oauthFlow{}, errFlowExpired
	}
	return f, nil
}
