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

// TestAgentOwnedSubsetOfRenamed — AgentOwned не должен разойтись с Renamed:
// каждое имя обязано быть реальным ключом карты (иначе CheckRenamed молча
// не находит для него новое имя — Renamed[k] на отсутствующем ключе вернёт
// "", и текст ошибки соврёт про "renamed to") и нести префикс
// GOTCHA_AGENT_ (иначе список перестаёт быть агентским по смыслу и
// internal/agent начнёт отказывать на переменной, которую не читает).
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

// TestCheckRenamedNilChecksWholeRegistry — nil-срез проверяет ВЕСЬ реестр
// (режим cmd/gotcha), не только AgentOwned. Подтест на КАЖДУЮ запись
// реестра — не таблица, по которой не итерируют.
func TestCheckRenamedNilChecksWholeRegistry(t *testing.T) {
	for _, old := range sortedRenamedOldNames() {
		newName := Renamed[old]
		t.Run(old, func(t *testing.T) {
			err := CheckRenamed(env(map[string]string{old: "x"}), nil)
			if err == nil {
				t.Fatalf("CheckRenamed(nil): want ошибку на %s, получили nil", old)
			}
			if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), newName) {
				t.Errorf("err = %q, want упоминание %s и %s", err, old, newName)
			}
		})
	}
}

// TestCheckRenamedScopedIgnoresOutOfScopeKeys — непустой `old` проверяет
// ТОЛЬКО перечисленные ключи: старое серверное имя, стоящее в общем .env,
// не должно ронять агента, которому оно не принадлежит (ops-review E3 T8
// круг 1) — CheckRenamed(getenv, AgentOwned) обязан вернуть nil, даже если
// getenv видит непустое значение постороннего (не входящего в AgentOwned)
// старого имени.
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
	err := CheckRenamed(env(map[string]string{outOfScope: "x"}), AgentOwned)
	if err != nil {
		t.Errorf("CheckRenamed(AgentOwned) на постороннем %s: %v, want nil", outOfScope, err)
	}
}

// TestCheckRenamedScopedCatchesInScopeKeys — при этом СВОИ ключи из `old`
// по-прежнему ловятся — сужение не глушит проверку целиком.
func TestCheckRenamedScopedCatchesInScopeKeys(t *testing.T) {
	for _, old := range AgentOwned {
		t.Run(old, func(t *testing.T) {
			err := CheckRenamed(env(map[string]string{old: "x"}), AgentOwned)
			if err == nil {
				t.Fatalf("CheckRenamed(AgentOwned): want ошибку на своём %s, получили nil", old)
			}
			if !strings.Contains(err.Error(), old) {
				t.Errorf("err = %q, want упоминание %s", err, old)
			}
		})
	}
}

// TestCheckRenamedEmptyValueLegit — пустое значение старого имени не роняет
// старт (docker-compose штатно прокидывает объявленные, но не заданные
// переменные пустой строкой).
func TestCheckRenamedEmptyValueLegit(t *testing.T) {
	old := sortedRenamedOldNames()[0]
	if err := CheckRenamed(env(map[string]string{old: ""}), nil); err != nil {
		t.Errorf("CheckRenamed с пустым %s: %v, want nil", old, err)
	}
}

// TestCheckRenamedListsAllFindings — несколько устаревших переменных сразу
// обязаны попасть в сообщение все, а не только первая встреченная.
func TestCheckRenamedListsAllFindings(t *testing.T) {
	names := sortedRenamedOldNames()
	old1, old2 := names[0], names[1]
	err := CheckRenamed(env(map[string]string{old1: "x", old2: "y"}), nil)
	if err == nil {
		t.Fatal("CheckRenamed: want ошибку на двух устаревших именах, получили nil")
	}
	for _, old := range []string{old1, old2} {
		if !strings.Contains(err.Error(), old) {
			t.Errorf("err = %q, want упоминание %s", err, old)
		}
	}
}
