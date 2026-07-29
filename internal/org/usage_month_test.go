package org

import (
	"testing"
	"time"
)

// TestMonthStartIgnoresCallerZone — период считается по UTC-календарю,
// независимо от того, в какой зоне пришёл момент.
//
// Раньше monthStart читал год и месяц в зоне АРГУМЕНТА и штамповал результат
// как UTC. Вызывающие разные: приём считает квоту от локальных часов, счётчик
// отброшенного — от time.Now().UTC(), страница организации — от локальных. На
// инстансе с TZ=Europe/Moscow в последние три часа месяца это давало РАЗНЫЕ
// строки org_usage для одного и того же момента: принятое писалось в один
// месяц, отброшенное из того же запроса — в другой.
func TestMonthStartIgnoresCallerZone(t *testing.T) {
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	// 2026-07-31 22:00 UTC — это уже 2026-08-01 01:00 по Москве.
	moment := time.Date(2026, 7, 31, 22, 0, 0, 0, time.UTC)
	want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if got := monthStart(moment); !got.Equal(want) {
		t.Errorf("monthStart(UTC) = %v, want %v", got, want)
	}
	if got := monthStart(moment.In(msk)); !got.Equal(want) {
		t.Errorf("monthStart(MSK) = %v, want %v — период не должен зависеть от зоны вызывающего", got, want)
	}

	// Зеркальный случай: зона позади UTC.
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	firstOfMonth := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC) // 31 июля 20:00 в LA
	wantAug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if got := monthStart(firstOfMonth.In(la)); !got.Equal(wantAug) {
		t.Errorf("monthStart(LA) = %v, want %v", got, wantAug)
	}
}
