package telemetry_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// Сверка идёт по фактическим полям PurgeResult и ключам SubjectExport.Counts, а не
// по переписанному в тесте перечню — переписанный список расходился бы молча с кодом.
func TestSubjectTablesCoverage(t *testing.T) {
	rt := reflect.TypeOf(telemetry.PurgeResult{})
	purgeNames := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		purgeNames[snakeCase(rt.Field(i).Name)] = true
	}
	if !purgeNames["spans"] {
		t.Fatalf("PurgeResult не несёт поля Spans — сверка ослепла, а не поле пропало")
	}

	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p := telemetry.NewPurger(conn)
	exp, err := p.ExportSubject(ctx, 900, telemetry.Subject{UserID: "nobody"})
	if err != nil {
		t.Fatalf("ExportSubject: %v", err)
	}

	for n := range purgeNames {
		if _, ok := exp.Counts[n]; !ok {
			t.Errorf("PurgeResult несёт таблицу %q, а SubjectExport.Counts — нет: выгрузка и стирание расходятся", n)
		}
	}
	for n := range exp.Counts {
		if !purgeNames[n] {
			t.Errorf("SubjectExport.Counts несёт таблицу %q, а PurgeResult — нет: выгрузка и стирание расходятся", n)
		}
	}
}
