package service

import (
	"math"
	"testing"
)

func TestParseCPUQuantity(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    float64
		wantErr bool
	}{
		{"empty returns zero without error", "", 0, false},
		{"plain cores", "2", 2, false},
		{"decimal cores", "1.5", 1.5, false},
		{"milli-cores", "500m", 0.5, false},
		{"nano-cores from metrics-server", "1500000000n", 1.5, false},
		{"micro-cores", "1234u", 0.001234, false},
		{"zero is valid", "0", 0, false},
		// Edge cases that silently returned 0 in the old fmt.Sscanf parser.
		{"invalid string is rejected", "abc", 0, true},
		{"two decimal points is rejected", "1.5.2", 0, true},
		// Note: K8s ParseQuantity accepts bare suffixes as zero ("m" = 0m = 0
		// cores). Unusual, but documented — not our responsibility to reject.
		{"bare suffix parses as zero", "m", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCPUQuantity(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCPUQuantity(%q) expected error, got %v", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCPUQuantity(%q) unexpected error: %v", tc.input, err)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("parseCPUQuantity(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseMemoryQuantity(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    float64
		wantErr bool
	}{
		{"empty returns zero without error", "", 0, false},
		{"bare number is bytes", "1024", 1024, false},
		{"mebibytes", "512Mi", 512 * 1024 * 1024, false},
		{"gibibytes", "2Gi", 2 * 1024 * 1024 * 1024, false},
		{"kibibytes", "1Ki", 1024, false},
		{"decimal gibibytes", "1.5Gi", 1.5 * 1024 * 1024 * 1024, false},
		// Decimal SI suffixes — K8s does differentiate Ki vs K.
		{"kilobytes (decimal)", "1000k", 1000 * 1000, false},
		{"megabytes (decimal)", "1M", 1000 * 1000, false},
		// Catches the regression that motivated this refactor: "1" = 1 byte,
		// not 1 Gi. Parser must accept this (it is syntactically valid) even
		// though the form-level validator warns about it.
		{"bare 1 is one byte, not an error", "1", 1, false},
		// Edge cases that silently returned partial values in the old parser.
		{"random garbage is rejected", "not-a-quantity", 0, true},
		// Note: K8s ParseQuantity accepts bare suffixes as zero ("Gi" = 0 Gi
		// = 0 bytes). Unusual, but documented — not our responsibility.
		{"bare suffix parses as zero", "Gi", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMemoryQuantity(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMemoryQuantity(%q) expected error, got %v", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMemoryQuantity(%q) unexpected error: %v", tc.input, err)
			}
			// Memory values are large — relative tolerance is safer than absolute.
			if tc.want != 0 {
				rel := math.Abs(got-tc.want) / tc.want
				if rel > 1e-6 {
					t.Errorf("parseMemoryQuantity(%q) = %v, want %v (rel diff %.2e)", tc.input, got, tc.want, rel)
				}
			} else if got != 0 {
				t.Errorf("parseMemoryQuantity(%q) = %v, want 0", tc.input, got)
			}
		})
	}
}
