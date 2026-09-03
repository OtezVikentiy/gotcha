package baseurl

import (
	"strings"
	"testing"
)

func TestNormalizeEmptyPassesThrough(t *testing.T) {
	got, err := Normalize("GOTCHA_X", "")
	if err != nil {
		t.Fatalf("Normalize(\"\"): %v", err)
	}
	if got != "" {
		t.Errorf("Normalize(\"\") = %q, want empty", got)
	}
}

func TestNormalizeTrimsTrailingSlashes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://gotcha.example.com", "https://gotcha.example.com"},
		{"https://gotcha.example.com/", "https://gotcha.example.com"},
		{"https://gotcha.example.com///", "https://gotcha.example.com"},
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"https://gw.example.com/gotcha", "https://gw.example.com/gotcha"},
	} {
		got, err := Normalize("GOTCHA_X", tc.in)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeRejectsInvalid(t *testing.T) {
	for _, in := range []string{
		"gotcha.example.com", // без схемы
		"/app",               // без хоста
		"ftp://gotcha.example.com",
		"https://gotcha.example.com?token=1",
		"https://gotcha.example.com#frag",
		"https://",
	} {
		if _, err := Normalize("GOTCHA_X", in); err == nil {
			t.Errorf("Normalize(%q): want error, got nil", in)
		}
	}
}

func TestNormalizeErrorNamesVariable(t *testing.T) {
	_, err := Normalize("GOTCHA_MY_VAR", "ftp://gotcha.example.com")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "GOTCHA_MY_VAR") {
		t.Errorf("error = %q, want it to name GOTCHA_MY_VAR", got)
	}
}
