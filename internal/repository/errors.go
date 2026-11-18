package repository

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
)

// fromK8s makes the answers of the API server domain errors, their messages
// unchanged; other errors, and nil, as they are.
func fromK8s(err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return apperr.Wrap(apperr.ErrNotFound, err)
	case apierrors.IsAlreadyExists(err):
		return apperr.Wrap(apperr.ErrAlreadyExists, err)
	case apierrors.IsConflict(err):
		return apperr.Wrap(apperr.ErrConflict, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return apperr.Wrap(apperr.ErrInvalid, err)
	}
	return err
}
