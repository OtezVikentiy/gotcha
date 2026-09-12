package templates

import "testing"

func TestHostLabelTextValue(t *testing.T) {
	if got := hostLabelText("prod"); got != "prod" {
		t.Errorf("hostLabelText(%q) = %q, want %q", "prod", got, "prod")
	}
	if got := hostLabelText(""); got != "—" {
		t.Errorf("hostLabelText(%q) = %q, want %q", "", got, "—")
	}
}
