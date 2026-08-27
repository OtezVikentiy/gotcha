//go:build !linux

package export

import "testing"

// freeBytes на не-Linux платформе: честный ok=false — вызывающий обязан
// считать бюджет ТОЛЬКО по Config.DiskBudget, не выдумывая число (см.
// докблок diskfree_other.go).
func TestFreeBytesUnsupportedPlatform(t *testing.T) {
	free, ok, err := freeBytes(t.TempDir())
	if ok {
		t.Fatalf("freeBytes на не-Linux вернул ok=true — платформа не должна выдавать число, которого нет")
	}
	if err != nil {
		t.Errorf("freeBytes на не-Linux вернул ошибку %v, ожидали nil", err)
	}
	if free != 0 {
		t.Errorf("freeBytes на не-Linux вернул free=%d, ожидали 0", free)
	}
}
