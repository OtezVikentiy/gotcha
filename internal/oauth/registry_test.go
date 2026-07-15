package oauth

import (
	"context"
	"testing"
)

type stubProvider struct{ name string }

func (s stubProvider) Name() string                     { return s.name }
func (s stubProvider) DisplayName() string              { return s.name }
func (s stubProvider) AuthURL(_, _, _, _ string) string { return "" }
func (s stubProvider) Exchange(_ context.Context, _, _, _, _ string) (Identity, error) {
	return Identity{}, nil
}

func TestRegistry(t *testing.T) {
	r := NewRegistry(stubProvider{"oidc"}, stubProvider{"yandex"})
	if r.Empty() {
		t.Fatal("registry not empty")
	}
	if got := r.List(); len(got) != 2 || got[0].Name() != "oidc" || got[1].Name() != "yandex" {
		t.Fatalf("List order wrong: %+v", got)
	}
	if _, ok := r.Get("yandex"); !ok {
		t.Fatal("Get yandex")
	}
	if _, ok := r.Get("vk"); ok {
		t.Fatal("Get vk must be false")
	}
	if !NewRegistry().Empty() {
		t.Fatal("empty registry")
	}
}
