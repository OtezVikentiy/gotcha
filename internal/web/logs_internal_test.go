package web

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

// TestParseLogFilterNegations — задача 5 («исключающие фильтры логов»):
// q_not/severity_not/service_not/environment_not/attr_not собираются в
// f.Not, проходя через log.NormalizePredicates (пустые/пробельные значения
// и неизвестный уровень отбрасываются).
func TestParseLogFilterNegations(t *testing.T) {
	rng := TimeRange{From: time.Now().Add(-time.Hour), To: time.Now()}

	q := url.Values{
		"q_not":           {"buffered to a temporary file", "  ", ""},
		"severity_not":    {"debug", "verbose"},
		"service_not":     {"cron"},
		"environment_not": {"staging"},
		"attr_not":        {"source:nginx", "res:host.name:web-01", "безДвоеточия"},
	}
	f, _ := parseLogFilter(q, rng, 14)

	if len(f.Not) != 6 {
		t.Fatalf("получено %d отрицаний, ожидалось 6: %#v", len(f.Not), f.Not)
	}
	for _, p := range f.Not {
		if strings.TrimSpace(p.Value) == "" {
			t.Errorf("пустое значение просочилось: %#v", p)
		}
		if p.Value == "verbose" {
			t.Errorf("неизвестный уровень просочился: %#v", p)
		}
	}

	// Поле и Key восстанавливаются верно для обоих видов атрибутов.
	var sawAttr, sawResAttr bool
	for _, p := range f.Not {
		if p.Field == log.FieldAttr && p.Key == "source" && p.Value == "nginx" {
			sawAttr = true
		}
		if p.Field == log.FieldResourceAttr && p.Key == "host.name" && p.Value == "web-01" {
			sawResAttr = true
		}
	}
	if !sawAttr {
		t.Errorf("не нашли отрицание по log_attributes source:nginx: %#v", f.Not)
	}
	if !sawResAttr {
		t.Errorf("не нашли отрицание по resource_attrs host.name:web-01: %#v", f.Not)
	}
}

// TestParseLogFilterNegationsCapped — потолок maxNegativeConditions режет
// избыточные условия из URL (положительные параметры такого потолка не
// имеют, см. комментарий у константы).
func TestParseLogFilterNegationsCapped(t *testing.T) {
	rng := TimeRange{From: time.Now().Add(-time.Hour), To: time.Now()}
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, fmt.Sprintf("шум-%d", i))
	}
	f, _ := parseLogFilter(url.Values{"q_not": many}, rng, 14)
	if len(f.Not) != maxNegativeConditions {
		t.Fatalf("кап не применён: %d отрицаний, ожидалось %d", len(f.Not), maxNegativeConditions)
	}
}

// TestApplyPredicatesRoundTrip — круговой обход filterToPredicates→
// applyPredicates: страховка от расхождения прямого и обратного
// преобразований (нужны задачам 9 и 10 для сохранённых/дефолтных фильтров).
func TestApplyPredicatesRoundTrip(t *testing.T) {
	src := log.ListFilter{
		Severity: []string{log.SevError},
		Service:  "api",
		Query:    "timeout",
		Attrs:    []log.AttrFilter{{Key: "source", Value: "php"}},
		Not: []log.Predicate{
			{Field: log.FieldBody, Op: log.OpNotContains, Value: "buffered"},
			{Field: log.FieldService, Op: log.OpNeq, Value: "cron"},
		},
	}

	var back log.ListFilter
	applyPredicates(&back, filterToPredicates(src))

	if back.Service != src.Service || back.Query != src.Query {
		t.Fatalf("плоские поля не восстановились: %#v", back)
	}
	if len(back.Severity) != 1 || back.Severity[0] != log.SevError {
		t.Fatalf("severity не восстановился: %#v", back.Severity)
	}
	if len(back.Attrs) != 1 || back.Attrs[0] != src.Attrs[0] {
		t.Fatalf("attrs не восстановились: %#v", back.Attrs)
	}
	if len(back.Not) != len(src.Not) {
		t.Fatalf("отрицания не восстановились: %#v", back.Not)
	}
	for i, p := range src.Not {
		if back.Not[i] != p {
			t.Errorf("отрицание %d не совпало: получено %#v, ожидалось %#v", i, back.Not[i], p)
		}
	}
}

// TestApplyPredicatesDedupes — applyPredicates не должен накапливать дубли
// при повторном применении одного и того же положительного предиката
// (мультивыбор severity/attrs идемпотентен).
func TestApplyPredicatesDedupes(t *testing.T) {
	var f log.ListFilter
	preds := []log.Predicate{
		{Field: log.FieldSeverity, Op: log.OpEq, Value: log.SevError},
		{Field: log.FieldSeverity, Op: log.OpEq, Value: log.SevError},
		{Field: log.FieldAttr, Op: log.OpEq, Key: "source", Value: "php"},
		{Field: log.FieldAttr, Op: log.OpEq, Key: "source", Value: "php"},
	}
	applyPredicates(&f, preds)

	if len(f.Severity) != 1 {
		t.Fatalf("severity задвоился: %#v", f.Severity)
	}
	if len(f.Attrs) != 1 {
		t.Fatalf("attrs задвоились: %#v", f.Attrs)
	}
}
