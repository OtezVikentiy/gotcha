package export

import "testing"

// TestBuildMetaFilterCodeIssue — заявка с заданным ScopeIssueID обязана
// давать FilterCodeIssue независимо от остальных Params: id одной группы —
// самый узкий и самодостаточный признак области.
func TestBuildMetaFilterCodeIssue(t *testing.T) {
	job := Job{Kind: KindEvents, ScopeIssueID: 123, Params: Params{Status: "unresolved"}}
	got := BuildMeta(job)
	if got.ScopeIssueID != 123 {
		t.Errorf("ScopeIssueID = %d, want 123", got.ScopeIssueID)
	}
	if got.FilterCode != FilterCodeIssue {
		t.Errorf("FilterCode = %q, want %q", got.FilterCode, FilterCodeIssue)
	}
}

// TestBuildMetaFilterCodeFiltered — область «проект», сужена хотя бы одним
// из status/level/environment/query.
func TestBuildMetaFilterCodeFiltered(t *testing.T) {
	cases := []Params{
		{Status: "unresolved"},
		{Level: "error"},
		{Environment: "prod"},
		{Query: "boom"},
	}
	for _, p := range cases {
		got := BuildMeta(Job{Kind: KindIssues, Params: p})
		if got.FilterCode != FilterCodeFiltered {
			t.Errorf("Params=%+v: FilterCode = %q, want %q", p, got.FilterCode, FilterCodeFiltered)
		}
		if got.ScopeIssueID != 0 {
			t.Errorf("Params=%+v: ScopeIssueID = %d, want 0", p, got.ScopeIssueID)
		}
	}
}

// TestBuildMetaFilterCodeAll — область «проект», ни один из
// status/level/environment/query не задан (период не учитывается, см.
// докблок Meta.FilterCode).
func TestBuildMetaFilterCodeAll(t *testing.T) {
	got := BuildMeta(Job{Kind: KindIssues, Params: Params{}})
	if got.FilterCode != FilterCodeAll {
		t.Errorf("FilterCode = %q, want %q", got.FilterCode, FilterCodeAll)
	}
}

// TestBuildMetaPseudonymNoteOnlyForMaskedEvents — F1′: пометка о
// невозможности сопоставить псевдонимы между выгрузками обязана появляться
// РОВНО там, где user_id заменяется псевдонимом (Kind=events,
// IncludePII=false), и нигде больше — у issues user_id нет вовсе, а
// IncludePII=true отдаёт его сырым.
func TestBuildMetaPseudonymNoteOnlyForMaskedEvents(t *testing.T) {
	cases := []struct {
		name     string
		job      Job
		wantNote bool
	}{
		{"events, includePII=false", Job{Kind: KindEvents, IncludePII: false}, true},
		{"events, includePII=true", Job{Kind: KindEvents, IncludePII: true}, false},
		{"issues, includePII=false", Job{Kind: KindIssues, IncludePII: false}, false},
		{"issues, includePII=true", Job{Kind: KindIssues, IncludePII: true}, false},
	}
	for _, c := range cases {
		got := BuildMeta(c.job)
		gotNote := got.PseudonymNote != ""
		if gotNote != c.wantNote {
			t.Errorf("%s: PseudonymNote непусто = %v, want %v (значение: %q)", c.name, gotNote, c.wantNote, got.PseudonymNote)
		}
		if c.wantNote && got.PseudonymNote != PseudonymUniquenessNote {
			t.Errorf("%s: PseudonymNote = %q, want %q", c.name, got.PseudonymNote, PseudonymUniquenessNote)
		}
	}
}
