package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
)

// hostThresholdsFixtureVM — HostDetailVM с показательным набором режимов по
// каждому виду (B2, T6): disk — override (host), memory — inherit
// (project), load — off (host), silent — inherit (role "web"). Effective
// подобрана вручную (не через ThresholdResolver — тот покрыт
// internal/host/resolve_test.go, T4) так, чтобы соответствовать Override.
func hostThresholdsFixtureVM(canOperate bool, form FormState) HostDetailVM {
	diskEnabled := true
	diskThreshold := 0.5
	loadEnabled := false

	return HostDetailVM{
		ProjectID:  1,
		Host:       host.Host{ID: 42, Name: "web-01"},
		CanOperate: canOperate,
		Override: host.ThresholdOverride{
			DiskEnabled:   &diskEnabled,
			DiskThreshold: &diskThreshold,
			LoadEnabled:   &loadEnabled,
		},
		Effective: host.EffectiveSettings{
			Settings: host.Settings{
				DiskEnabled:     true,
				DiskThreshold:   0.5,
				MemoryEnabled:   true,
				MemoryThreshold: 0.9,
				LoadEnabled:     false,
				LoadThreshold:   4,
				SilentEnabled:   true,
				SilentAfter:     15 * time.Minute,
			},
			DiskSource:   host.ThresholdSource{Level: host.LevelHost},
			MemorySource: host.ThresholdSource{Level: host.LevelProject},
			LoadSource:   host.ThresholdSource{Level: host.LevelHost},
			SilentSource: host.ThresholdSource{Level: host.LevelRole, Label: "web"},
		},
		ThresholdsForm: form,
	}
}

func renderHostThresholds(t *testing.T, vm HostDetailVM) string {
	t.Helper()
	var sb strings.Builder
	if err := hostThresholdsSection(vm).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// TestHostThresholdsFormModes — оператору видна форма с тремя radio на вид
// порога (наследовать/переопределить/выключить), выбранный режим совпадает с
// Override, а числовое поле переопределения предзаполнено ДЕЙСТВУЮЩИМ
// значением (Effective), не нулём — переключение на "Переопределить" не
// начинается с чистого листа.
func TestHostThresholdsFormModes(t *testing.T) {
	out := renderHostThresholds(t, hostThresholdsFixtureVM(true, nil))

	if !strings.Contains(out, `class="host-thresholds-form"`) {
		t.Fatalf("нет формы порогов у оператора: %s", out)
	}
	// disk — override, checked на "override".
	if !strings.Contains(out, `name="disk_mode" value="override" checked`) {
		t.Errorf("disk_mode не выбран override: %s", out)
	}
	if strings.Contains(out, `name="disk_mode" value="inherit" checked`) {
		t.Errorf("disk_mode ошибочно выбран inherit: %s", out)
	}
	// memory — inherit (Override.MemoryEnabled == nil).
	if !strings.Contains(out, `name="memory_mode" value="inherit" checked`) {
		t.Errorf("memory_mode не выбран inherit: %s", out)
	}
	// load — off.
	if !strings.Contains(out, `name="load_mode" value="off" checked`) {
		t.Errorf("load_mode не выбран off: %s", out)
	}
	// silent — inherit.
	if !strings.Contains(out, `name="silent_mode" value="inherit" checked`) {
		t.Errorf("silent_mode не выбран inherit: %s", out)
	}
	// Значение поля переопределения диска — 50% (Override.DiskThreshold),
	// не 0.
	if !strings.Contains(out, `name="disk_value"`) || !strings.Contains(out, `value="50"`) {
		t.Errorf("disk_value не предзаполнен текущим override (50): %s", out)
	}
	// memory без override → предзаполнено ЭФФЕКТИВНЫМ (90), не нулём.
	if !strings.Contains(out, `name="memory_value"`) || !strings.Contains(out, `value="90"`) {
		t.Errorf("memory_value не предзаполнен эффективным значением (90): %s", out)
	}
}

// TestHostThresholdsSourceLabels — подпись «действует: значение (источник)»
// у каждого вида: host/project по уровню без метки, role — с меткой ("web"),
// выключенный вид (load) показывает "выключено", а не число.
func TestHostThresholdsSourceLabels(t *testing.T) {
	out := renderHostThresholds(t, hostThresholdsFixtureVM(true, nil))

	if !strings.Contains(out, "50.0%") {
		t.Errorf("нет значения диска в процентах (источник host): %s", out)
	}
	if !strings.Contains(out, "90.0%") {
		t.Errorf("нет значения памяти в процентах (источник project): %s", out)
	}
	if !strings.Contains(out, "«web»") {
		t.Errorf("нет метки роли-источника у тишины: %s", out)
	}
	if !strings.Contains(out, "выключено") {
		t.Errorf("выключенный load не показан как «выключено»: %s", out)
	}
}

// TestHostThresholdsReadonlyForNonOperator — не-оператору форма не
// показывается (нет ни одного input), только список эффективных значений
// (тот же источник, что видел бы оператор).
func TestHostThresholdsReadonlyForNonOperator(t *testing.T) {
	out := renderHostThresholds(t, hostThresholdsFixtureVM(false, nil))

	if strings.Contains(out, "<form") {
		t.Errorf("не-оператору показана форма порогов: %s", out)
	}
	if strings.Contains(out, "<input") {
		t.Errorf("не-оператору показаны input'ы порогов: %s", out)
	}
	if !strings.Contains(out, `class="host-thresholds-readonly"`) {
		t.Errorf("нет readonly-списка порогов: %s", out)
	}
	if !strings.Contains(out, "50.0%") {
		t.Errorf("readonly-список не показывает эффективное значение диска: %s", out)
	}
}

// TestHostThresholdsErrorAndFormState — при ошибке валидации (422) карточка
// переотрисовывается с сообщением и введёнными (а не старыми) значениями
// формы — тот же приём, что у HostSettings/FormState.
func TestHostThresholdsErrorAndFormState(t *testing.T) {
	form := FormState{
		"disk_mode":  "override",
		"disk_value": "150", // невалидное значение, но должно остаться в форме
	}
	vm := hostThresholdsFixtureVM(true, form)
	vm.ThresholdsErr = "порог диска должен быть от 1 до 100%"

	out := renderHostThresholds(t, vm)
	if !strings.Contains(out, "порог диска должен быть от 1 до 100%") {
		t.Errorf("нет сообщения об ошибке: %s", out)
	}
	if !strings.Contains(out, `value="150"`) {
		t.Errorf("введённое (невалидное) значение не сохранено в форме: %s", out)
	}
}
