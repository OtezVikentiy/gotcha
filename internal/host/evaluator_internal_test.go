package host

import "testing"

func TestRotateHosts(t *testing.T) {
	hosts := []Host{
		{ProjectID: 1, Name: "a"},
		{ProjectID: 1, Name: "b"},
		{ProjectID: 2, Name: "a"},
	}

	if got := rotateHosts(nil, hostKey{}); got != nil {
		t.Errorf("rotateHosts(nil) = %v, want nil", got)
	}

	cases := []struct {
		name   string
		cursor hostKey
		want   []hostKey
	}{
		{
			"курсор нулевой — обход с начала",
			hostKey{},
			[]hostKey{{1, "a"}, {1, "b"}, {2, "a"}},
		},
		{
			"курсор на первом — начинаем со второго",
			hostKey{1, "a"},
			[]hostKey{{1, "b"}, {2, "a"}, {1, "a"}},
		},
		{
			"курсор на среднем",
			hostKey{1, "b"},
			[]hostKey{{2, "a"}, {1, "a"}, {1, "b"}},
		},
		{
			"курсор на последнем — полный круг",
			hostKey{2, "a"},
			[]hostKey{{1, "a"}, {1, "b"}, {2, "a"}},
		},
		{
			"курсор за пределами списка (хост удалён) — оборачиваем как после последнего",
			hostKey{9, "z"},
			[]hostKey{{1, "a"}, {1, "b"}, {2, "a"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rotateHosts(hosts, tc.cursor)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i, want := range tc.want {
				if hostKeyOf(got[i]) != want {
					t.Errorf("rotateHosts(%v)[%d] = %v, want %v", tc.cursor, i, hostKeyOf(got[i]), want)
				}
			}
		})
	}
}
