package main

import (
	"strings"
	"testing"
)

func TestDispatchSubcommandIgnoresFlagsAndUnknownWords(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"--version"},
		{"--healthcheck"},
		{"version"},
		{"bogus-subcommand"},
	}
	for _, args := range cases {
		if _, handled := dispatchSubcommand(args, func(string) string { return "" }); handled {
			t.Errorf("dispatchSubcommand(%v) handled = true, want false", args)
		}
	}
}

func TestDispatchSubcommandRoutesSetPassword(t *testing.T) {
	// email отсутствует — код обязан быть от разбора аргументов (2), а не от run(): диспетчер
	// обязан перехватить "set-password" и не пропустить его дальше в обычный запуск сервера.
	code, handled := dispatchSubcommand([]string{"set-password"}, func(string) string { return "" })
	if !handled {
		t.Fatal("dispatchSubcommand([\"set-password\"]) handled = false, want true")
	}
	if code != 2 {
		t.Errorf("code = %d, want 2 (missing --email)", code)
	}
}

func TestParseSetPasswordArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{"equals form", []string{"--email=a@example.com"}, "a@example.com", ""},
		{"space form", []string{"--email", "a@example.com"}, "a@example.com", ""},
		{"missing email", nil, "", "--email is required"},
		{"password as argv equals", []string{"--email=a@example.com", "--password=x"}, "", "never a command-line argument"},
		{"password as argv space", []string{"--email=a@example.com", "--password", "x"}, "", "never a command-line argument"},
		{"unknown flag", []string{"--email=a@example.com", "--bogus"}, "", "unrecognized argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSetPasswordArgs(c.args)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("email = %q, want %q", got, c.want)
			}
		})
	}
}

func TestReadPassword(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"trailing newline stripped", "hunter2\n", "hunter2", false},
		{"crlf stripped", "hunter2\r\n", "hunter2", false},
		{"no trailing newline (EOF)", "hunter2", "hunter2", false},
		{"internal spaces kept", "a password with spaces\n", "a password with spaces", false},
		{"only first line used", "first\nsecond\n", "first", false},
		{"empty input rejected", "", "", true},
		{"blank line rejected", "\n", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readPassword(strings.NewReader(c.in))
			if c.wantErr {
				if err == nil {
					t.Fatalf("readPassword(%q): want error, got nil (got=%q)", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readPassword(%q): unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("readPassword(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
