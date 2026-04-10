package models

import "testing"

func TestMapPhaseToStatus(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		want  string
	}{
		{"empty phase means installing", "", "Installing"},
		{"ready stays ready", "READY", "Ready"},
		{"error maps to error", "ERROR", "Error"},
		{"failed maps to error", "FAILED", "Error"},
		{"wait-helm-release is installing", "WAIT_HREL", "Installing"},
		{"wait-parent is installing", "WAIT_PRT", "Installing"},
		{"wait-oci is installing", "WAIT_OCI", "Installing"},
		{"installing is installing", "INSTALLING", "Installing"},
		{"upgrading stays updating", "UPGRADING", "Updating"},
		{"unknown phase falls back to installing, not leaked raw", "SOME_NEW_PHASE", "Installing"},
		{"case-sensitive: lowercase unknown", "ready", "Installing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MapPhaseToStatus(tc.phase); got != tc.want {
				t.Errorf("MapPhaseToStatus(%q) = %q, want %q", tc.phase, got, tc.want)
			}
		})
	}
}
