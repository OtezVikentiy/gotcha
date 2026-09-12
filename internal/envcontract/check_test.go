package envcontract

import (
	"sort"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func sortedRenamedOldNames() []string {
	names := make([]string, 0, len(Renamed))
	for old := range Renamed {
		names = append(names, old)
	}
	sort.Strings(names)
	return names
}

func TestAgentOwnedSubsetOfRenamed(t *testing.T) {
	if len(AgentOwned) == 0 {
		t.Fatal("AgentOwned пуст")
	}
	for _, old := range AgentOwned {
		if _, ok := Renamed[old]; !ok {
			t.Errorf("AgentOwned содержит %s, которой нет среди ключей Renamed", old)
		}
		if !strings.HasPrefix(old, "GOTCHA_AGENT_") {
			t.Errorf("AgentOwned содержит %s без префикса GOTCHA_AGENT_", old)
		}
	}
}

func TestInfraOwnedSubsetOfRenamed(t *testing.T) {
	if len(InfraOwned) == 0 {
		t.Fatal("InfraOwned пуст")
	}
	for _, old := range InfraOwned {
		newName, ok := Renamed[old]
		if !ok {
			t.Errorf("InfraOwned содержит %s, которой нет среди ключей Renamed", old)
			continue
		}
		if !strings.HasPrefix(newName, "GOTCHA_COMPOSE_") && !strings.HasPrefix(newName, "GOTCHA_BUILD_") {
			t.Errorf("InfraOwned: Renamed[%s] = %s без префикса GOTCHA_COMPOSE_/GOTCHA_BUILD_", old, newName)
		}
	}
}

func TestCheckRenamedAllChecksWholeRegistry(t *testing.T) {
	// Длина сверяется напрямую с len(Renamed), а не через саму функцию —
	// иначе урезанный обход остался бы валидным []string и тест не заметил бы.
	if got, want := len(sortedRenamedOldNames()), len(Renamed); got != want {
		t.Fatalf("sortedRenamedOldNames() вернула %d имён, Renamed содержит %d — обход урезан, ниже проверится не весь реестр", got, want)
	}
	for _, old := range sortedRenamedOldNames() {
		newName := Renamed[old]
		t.Run(old, func(t *testing.T) {
			err := CheckRenamedAll(env(map[string]string{old: "x"}))
			if err == nil {
				t.Fatalf("CheckRenamedAll: want ошибку на %s, получили nil", old)
			}
			if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), newName) {
				t.Errorf("err = %q, want упоминание %s и %s", err, old, newName)
			}
		})
	}
}

func TestCheckRenamedScopedIgnoresOutOfScopeKeys(t *testing.T) {
	outOfScope := ""
	for _, old := range sortedRenamedOldNames() {
		found := false
		for _, a := range AgentOwned {
			if a == old {
				found = true
				break
			}
		}
		if !found {
			outOfScope = old
			break
		}
	}
	if outOfScope == "" {
		t.Fatal("обход ослеп: в реестре не нашлось имени вне AgentOwned")
	}
	err := CheckRenamedScoped(env(map[string]string{outOfScope: "x"}), AgentOwned)
	if err != nil {
		t.Errorf("CheckRenamedScoped(AgentOwned) на постороннем %s: %v, want nil", outOfScope, err)
	}
}

func TestCheckRenamedScopedCatchesInScopeKeys(t *testing.T) {
	for _, old := range AgentOwned {
		t.Run(old, func(t *testing.T) {
			err := CheckRenamedScoped(env(map[string]string{old: "x"}), AgentOwned)
			if err == nil {
				t.Fatalf("CheckRenamedScoped(AgentOwned): want ошибку на своём %s, получили nil", old)
			}
			if !strings.Contains(err.Error(), old) {
				t.Errorf("err = %q, want упоминание %s", err, old)
			}
		})
	}
}

func TestCheckRenamedScopedEmptySetChecksNothingDeliberately(t *testing.T) {
	old := sortedRenamedOldNames()[0]
	getenv := env(map[string]string{old: "x"})
	if err := CheckRenamedScoped(getenv, []string{}); err != nil {
		t.Errorf("CheckRenamedScoped(getenv, []string{}) на env с устаревшим %s: %v, want nil (пустой набор — не весь реестр)", old, err)
	}
	if err := CheckRenamedScoped(getenv, nil); err != nil {
		t.Errorf("CheckRenamedScoped(getenv, nil) на env с устаревшим %s: %v, want nil (nil — тоже пустой набор, не весь реестр)", old, err)
	}
}

func TestCheckRenamedEmptyValueLegit(t *testing.T) {
	old := sortedRenamedOldNames()[0]
	if err := CheckRenamedAll(env(map[string]string{old: ""})); err != nil {
		t.Errorf("CheckRenamedAll с пустым %s: %v, want nil", old, err)
	}
}

func TestCheckRenamedListsAllFindings(t *testing.T) {
	names := sortedRenamedOldNames()
	old1, old2 := names[0], names[1]
	err := CheckRenamedAll(env(map[string]string{old1: "x", old2: "y"}))
	if err == nil {
		t.Fatal("CheckRenamedAll: want ошибку на двух устаревших именах, получили nil")
	}
	for _, old := range []string{old1, old2} {
		if !strings.Contains(err.Error(), old) {
			t.Errorf("err = %q, want упоминание %s", err, old)
		}
	}
}

// Ловит переименование уже переименованного: если будущая волна переименует
// B (цель старой записи A→B) в C, запись A→B молча указывает на B, которого уже нет в Known.
func TestRenamedTargetsAreKnown(t *testing.T) {
	infra := map[string]bool{}
	for _, old := range InfraOwned {
		infra[old] = true
	}
	for old, newName := range Renamed {
		if infra[old] {
			continue
		}
		if !Known[newName] {
			t.Errorf("Renamed[%s] = %s, но %s отсутствует в Known — переименование ведёт в никуда", old, newName, newName)
		}
	}
}
