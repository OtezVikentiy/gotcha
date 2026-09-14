package guards

import (
	"strings"
	"testing"
)

// Шорткат animation в reduced-motion сбрасывает animation-play-state в running и снимает
// паузу по :hover/:focus-within — разрешён только longhand, не задевающий play-state.
func TestReducedMotionFlashDoesNotResetPlayState(t *testing.T) {
	tree := Load(t)
	blocks := parseCSSBlocks(tree.CSS.Body)

	var hoverPauses, mediaOverride bool
	for _, b := range blocks {
		switch {
		case b.AtRule == "" && b.Selector == ".flash:hover, .flash:focus-within":
			if strings.Contains(b.Body, "animation-play-state") && strings.Contains(b.Body, "paused") {
				hoverPauses = true
			}
		case strings.Contains(b.AtRule, "prefers-reduced-motion: reduce") && b.Selector == ".js .flash":
			mediaOverride = true
			if strings.Contains(b.Body, "animation:") {
				t.Errorf("app.css:%d: %s использует шорткат animation внутри prefers-reduced-motion — он сбрасывает animation-play-state в running и снимает паузу по hover/focus", b.Line, b.Selector)
			}
		}
	}
	if !hoverPauses {
		t.Fatal(".flash:hover, .flash:focus-within с animation-play-state: paused не найден — разбор сломан или правило пропало")
	}
	if !mediaOverride {
		t.Fatal("@media (prefers-reduced-motion: reduce) .js .flash не найден — разбор сломан или правило пропало")
	}
}
