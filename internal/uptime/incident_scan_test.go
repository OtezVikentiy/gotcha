package uptime

import (
	"strings"
	"testing"
)

// TestIncidentScanDestMatchesColumns — сторож против расхождения списка
// приёмников с истиной: истина здесь incidentColumns (её видит PostgreSQL),
// копия — incidentScanDest. Разошлись они уже однажды на колонке
// notify_open_channels (миграция 0086), и цена была 500 на обеих страницах
// с инцидентами; ловится это без базы простым сравнением длин.
func TestIncidentScanDestMatchesColumns(t *testing.T) {
	want := len(strings.Split(incidentColumns, ","))
	if got := len(incidentScanDest(&Incident{})); got != want {
		t.Errorf("приёмников в incidentScanDest = %d, колонок в incidentColumns = %d", got, want)
	}
}
