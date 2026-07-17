package theme

import (
	"context"
	"testing"
)

func TestThemeContextAndParse(t *testing.T) {
	if FromContext(context.Background()).Code != "system" {
		t.Fatal("default not system")
	}
	ctx := WithTheme(context.Background(), Theme{Code: "light"})
	if FromContext(ctx).Code != "light" {
		t.Fatal("ctx theme")
	}
	for _, c := range []string{"dark", "light", "system"} {
		if _, ok := Parse(c); !ok {
			t.Fatalf("Parse(%q)", c)
		}
	}
	if _, ok := Parse("neon"); ok {
		t.Fatal("Parse(neon) should be !ok")
	}
}
