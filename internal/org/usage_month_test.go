package org

import (
	"testing"
	"time"
)

func TestMonthStartIgnoresCallerZone(t *testing.T) {
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	moment := time.Date(2026, 7, 31, 22, 0, 0, 0, time.UTC)
	want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if got := monthStart(moment); !got.Equal(want) {
		t.Errorf("monthStart(UTC) = %v, want %v", got, want)
	}
	if got := monthStart(moment.In(msk)); !got.Equal(want) {
		t.Errorf("monthStart(MSK) = %v, want %v — период не должен зависеть от зоны вызывающего", got, want)
	}

	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	firstOfMonth := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	wantAug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if got := monthStart(firstOfMonth.In(la)); !got.Equal(wantAug) {
		t.Errorf("monthStart(LA) = %v, want %v", got, wantAug)
	}
}
