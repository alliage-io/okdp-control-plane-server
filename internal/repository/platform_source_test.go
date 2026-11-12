package repository

import (
	"context"
	"strings"
	"testing"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-gitops/gitops"
)

func TestMissingKeysNameThePlatformValues(t *testing.T) {
	repo := newContextWith(t, map[string]interface{}{})

	cases := []struct {
		name string
		call func() error
	}{
		{"ingress.suffix", func() error {
			_, err := repo.GetIngressSuffix(context.Background())
			return err
		}},
		{"sparkOperator", func() error {
			_, err := repo.GetSparkConfig(context.Background())
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected an error naming the platform values")
			}
			if !strings.Contains(err.Error(), "global.okdp") {
				t.Errorf("expected global.okdp to be named, got %q", err.Error())
			}
		})
	}
}

// The platform values are platform.yaml, in Git.
func TestPlatformValuesAreThePlatformFile(t *testing.T) {
	store := gitops.MustMemoryStore(map[string]string{
		compiler.PlatformFile: "global:\n  okdp:\n    ingress:\n      suffix: okdp.sandbox\n",
	})
	repo := NewPlatformRepository(gitops.NewDeployments(store))
	suffix, err := repo.GetIngressSuffix(context.Background())
	if err != nil || suffix != "okdp.sandbox" {
		t.Fatalf("suffix = %q, err = %v", suffix, err)
	}
}

func TestNoPlatformValuesAtAllIsAnError(t *testing.T) {
	repo := NewPlatformRepository(gitops.NewDeployments(gitops.MustMemoryStore(nil)))
	if _, err := repo.GetIngressSuffix(context.Background()); err == nil || !strings.Contains(err.Error(), compiler.PlatformFile) {
		t.Fatalf("err = %v, want one naming %s", err, compiler.PlatformFile)
	}
}
