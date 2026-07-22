package main

import "testing"

func TestVersionRequested(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--version"}, true},
		{[]string{"version"}, true},
		{[]string{"--mode=web", "--version"}, true},
		{[]string{"--mode=all"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := versionRequested(c.args); got != c.want {
			t.Errorf("versionRequested(%v) = %v, ждали %v", c.args, got, c.want)
		}
	}
}
