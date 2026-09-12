package templates

import "testing"

func TestSubjectPurgeVMInertKey(t *testing.T) {
	cases := []struct {
		name string
		vm   SubjectPurgeVM
		want string
	}{
		{"оба критерия рабочие", SubjectPurgeVM{}, ""},
		{"обезличен только email", SubjectPurgeVM{InertEmail: true}, "org.gdpr.purge.inert_email"},
		{"обезличен только IP", SubjectPurgeVM{InertIP: true}, "org.gdpr.purge.inert_ip"},
		{"обезличены оба", SubjectPurgeVM{InertEmail: true, InertIP: true}, "org.gdpr.purge.inert"},
	}
	for _, c := range cases {
		if got := c.vm.InertKey(); got != c.want {
			t.Errorf("%s: InertKey() = %q, want %q", c.name, got, c.want)
		}
	}
}
