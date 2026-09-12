package uptime

import (
	"strings"
	"testing"
)

func TestIncidentScanDestMatchesColumns(t *testing.T) {
	want := len(strings.Split(incidentColumns, ","))
	if got := len(incidentScanDest(&Incident{})); got != want {
		t.Errorf("приёмников в incidentScanDest = %d, колонок в incidentColumns = %d", got, want)
	}
}
