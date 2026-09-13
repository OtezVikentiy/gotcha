package guards

import (
	"strings"
	"testing"
)

// Промер на 360px: имени монитора доставалось 122-142px из нужных 198-270 — без переноса
// оно обрезалось посередине FQDN.
func TestStatusTileNameWrapsOnPhone(t *testing.T) {
	tree := Load(t)
	blocks := parseCSSBlocks(tree.CSS.Body)

	var headWraps, nameFull bool
	for _, b := range blocks {
		if !strings.Contains(b.AtRule, "max-width: 560px") {
			continue
		}
		switch b.Selector {
		case ".status-tile-head":
			if strings.Contains(b.Body, "flex-wrap: wrap") {
				headWraps = true
			}
		case ".status-tile-name":
			if strings.Contains(b.Body, "white-space: normal") {
				nameFull = true
			}
		}
	}
	if !headWraps {
		t.Error(".status-tile-head в @media (max-width: 560px) должен получать flex-wrap: wrap — иначе бейдж не уступает место имени")
	}
	if !nameFull {
		t.Error(".status-tile-name в @media (max-width: 560px) должен снимать white-space: nowrap — иначе длинное имя монитора обрезается")
	}
}

// Промер на 360px: .chip как flex-item колонки растягивается по align-items: stretch
// (было 205px при 19px текста) — нужен align-self или width: fit-content.
func TestProjectCardChipNotStretched(t *testing.T) {
	tree := Load(t)
	blocks := parseCSSBlocks(tree.CSS.Body)

	var found bool
	for _, b := range blocks {
		if b.Selector != ".project-card .chip" {
			continue
		}
		found = true
		if !strings.Contains(b.Body, "align-self") && !strings.Contains(b.Body, "width: fit-content") {
			t.Error(".project-card .chip должен снимать растяжение по кросс-оси (align-self или width: fit-content)")
		}
	}
	if !found {
		t.Fatal(".project-card .chip не найден — разбор сломан или правило пропало")
	}
}
