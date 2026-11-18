package service

import (
	"errors"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func TestNormalizeAndValidateCatalogService(t *testing.T) {
	cases := []struct {
		name        string
		in          models.PlatformService
		wantErr     bool
		wantDefault string // expected DefaultVersion when wantErr is false
	}{
		{
			name:        "leaves the default to the catalogs when omitted",
			in:          models.PlatformService{Name: "trino", Versions: []string{"0.3.0", "0.2.0"}, Category: "Interactive Query"},
			wantDefault: "",
		},
		{
			name:        "no versions: the catalogs'",
			in:          models.PlatformService{Name: "trino", Category: "Interactive Query"},
			wantDefault: "",
		},
		{
			name:        "keeps a valid explicit default",
			in:          models.PlatformService{Name: "trino", Versions: []string{"0.3.0", "0.2.0"}, DefaultVersion: "0.2.0", Category: "Interactive Query"},
			wantDefault: "0.2.0",
		},
		{
			name:        "trims surrounding whitespace from name",
			in:          models.PlatformService{Name: "  trino  ", Versions: []string{"0.1.0"}, DefaultVersion: "0.1.0", Category: "Interactive Query"},
			wantDefault: "0.1.0",
		},
		{"empty name is rejected", models.PlatformService{Name: "", Versions: []string{"0.1.0"}}, true, ""},
		{"missing category is rejected", models.PlatformService{Name: "trino", Versions: []string{"0.1.0"}}, true, ""},
		{"non-DNS name is rejected", models.PlatformService{Name: "Trino_X", Versions: []string{"0.1.0"}}, true, ""},
		{"uppercase name is rejected", models.PlatformService{Name: "Trino", Versions: []string{"0.1.0"}}, true, ""},
		{"empty version string is rejected", models.PlatformService{Name: "trino", Versions: []string{" "}, Category: "Q"}, true, ""},
		{"duplicate versions are rejected", models.PlatformService{Name: "trino", Versions: []string{"0.1.0", "0.1.0"}, Category: "Q"}, true, ""},
		{"default not in versions is rejected", models.PlatformService{Name: "trino", Versions: []string{"0.1.0"}, DefaultVersion: "9.9.9", Category: "Q"}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := tc.in
			err := normalizeAndValidateCatalogService(&svc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (svc=%+v)", svc)
				}
				if !errors.Is(err, ErrCatalogValidation) {
					t.Fatalf("expected ErrCatalogValidation, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if svc.DefaultVersion != tc.wantDefault {
				t.Errorf("DefaultVersion = %q, want %q", svc.DefaultVersion, tc.wantDefault)
			}
			if svc.Name != "trino" {
				t.Errorf("Name = %q, want %q (should be trimmed)", svc.Name, "trino")
			}
		})
	}
}
