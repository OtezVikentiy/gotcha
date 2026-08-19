package templates

import "testing"

// TestHostLabelTextValue — hostLabelText вызывается в hostsTable (hosts.templ)
// только с пустой строкой: непустая метка окружения/роли рисуется бейджем в
// отдельной ветке шаблона, а не через эту функцию. Ветка «есть значение» (см.
// докблок hostLabelText в hosts.templ) остаётся непокрытой рендер-тестами
// пакета web — закрываем прямым вызовом здесь, в том же пакете.
func TestHostLabelTextValue(t *testing.T) {
	if got := hostLabelText("prod"); got != "prod" {
		t.Errorf("hostLabelText(%q) = %q, want %q", "prod", got, "prod")
	}
	if got := hostLabelText(""); got != "—" {
		t.Errorf("hostLabelText(%q) = %q, want %q", "", got, "—")
	}
}
