package apperr

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The messages are those of the API server's errors these replaced: the
// console shows them.
func TestTheMessagesAreTheAPIServers(t *testing.T) {
	gr := schema.GroupResource{Group: "okdp.io", Resource: "services"}
	cause := errors.New("the head moved")
	for _, tc := range []struct {
		got, want error
		kind      error
	}{
		{NotFound("services.okdp.io", "x"), apierrors.NewNotFound(gr, "x"), ErrNotFound},
		{AlreadyExists("services.okdp.io", "x"), apierrors.NewAlreadyExists(gr, "x"), ErrAlreadyExists},
		{Conflict("services.okdp.io", "x", cause), apierrors.NewConflict(gr, "x", cause), ErrConflict},
		{Invalid("name is required"), apierrors.NewBadRequest("name is required"), ErrInvalid},
	} {
		if tc.got.Error() != tc.want.Error() {
			t.Errorf("%q, want %q", tc.got, tc.want)
		}
		if !errors.Is(fmt.Errorf("wrapped: %w", tc.got), tc.kind) {
			t.Errorf("%q is not %v", tc.got, tc.kind)
		}
	}
}

func TestWrapKeepsTheCause(t *testing.T) {
	cause := errors.New("boom")
	err := Wrap(ErrNotFound, cause)
	if err.Error() != "boom" || !errors.Is(err, ErrNotFound) || !errors.Is(err, cause) {
		t.Fatalf("got %v", err)
	}
}
