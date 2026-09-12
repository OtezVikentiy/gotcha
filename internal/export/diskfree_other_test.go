//go:build !linux

package export

import "testing"

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
