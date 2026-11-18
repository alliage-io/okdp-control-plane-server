package repository

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
)

func TestFromK8sMakesDomainErrors(t *testing.T) {
	gr := schema.GroupResource{Resource: "secrets"}
	for _, tc := range []struct {
		err  error
		kind error
	}{
		{apierrors.NewNotFound(gr, "x"), apperr.ErrNotFound},
		{apierrors.NewAlreadyExists(gr, "x"), apperr.ErrAlreadyExists},
		{apierrors.NewConflict(gr, "x", errors.New("c")), apperr.ErrConflict},
		{apierrors.NewBadRequest("b"), apperr.ErrInvalid},
		{apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, "x", nil), apperr.ErrInvalid},
	} {
		got := fromK8s(fmt.Errorf("%w", tc.err))
		if !errors.Is(got, tc.kind) || got.Error() != tc.err.Error() {
			t.Errorf("%v: got %v", tc.err, got)
		}
	}
	if fromK8s(nil) != nil {
		t.Error("nil became an error")
	}
	other := errors.New("boom")
	if fromK8s(other) != other {
		t.Error("another error was changed")
	}
}
