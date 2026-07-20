package web

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
)

func TestFlamegraphSVG(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{
		{Name: "aaaaaaaaaa", Value: 6},
		{Name: "bbbbbbbbbb", Value: 4},
	}}
	var sb strings.Builder
	_ = flamegraphSVG(context.Background(), root, 600).Render(context.Background(), &sb)
	out := sb.String()
	if !strings.Contains(out, "<svg") || !strings.Contains(out, "<rect") {
		t.Fatalf("svg missing rects: %s", out)
	}
	if !strings.Contains(out, "aaaaaaaaaa") || !strings.Contains(out, "bbbbbbbbbb") {
		t.Fatalf("svg missing frame names: %s", out)
	}
}

func TestFlamegraphSVGEmpty(t *testing.T) {
	var sb strings.Builder
	_ = flamegraphSVG(context.Background(), &profile.FlameNode{Name: "all", Value: 0}, 600).Render(context.Background(), &sb)
	if !strings.Contains(sb.String(), "нет данных") {
		t.Fatalf("empty tree should render placeholder: %s", sb.String())
	}
}
