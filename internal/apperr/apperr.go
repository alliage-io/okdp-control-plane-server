// Package apperr holds the domain errors of the server: what the repositories
// and services report, whatever their backend (Git, Keycloak, Kubernetes),
// and the HTTP layer translates to a status (handlers.writeError). Compare
// with errors.Is.
package apperr

import (
	"errors"
	"fmt"
)

// The kinds of a domain error.
var (
	// ErrNotFound is a resource that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists is a resource already there under that name.
	ErrAlreadyExists = errors.New("already exists")
	// ErrConflict is a write losing to another one.
	ErrConflict = errors.New("conflict")
	// ErrInvalid is a request refused for what it asks.
	ErrInvalid = errors.New("invalid")
)

// domainError is an error of a kind, with its own message.
type domainError struct {
	kind    error
	message string
	cause   error
}

func (e *domainError) Error() string { return e.message }

func (e *domainError) Is(target error) bool { return target == e.kind }

func (e *domainError) Unwrap() error { return e.cause }

// NotFound is a missing resource (a qualified name: "services.okdp.io").
func NotFound(resource, name string) error {
	return &domainError{kind: ErrNotFound, message: fmt.Sprintf("%s %q not found", resource, name)}
}

// AlreadyExists is a resource already there.
func AlreadyExists(resource, name string) error {
	return &domainError{kind: ErrAlreadyExists, message: fmt.Sprintf("%s %q already exists", resource, name)}
}

// Conflict is a write of resource that lost to another one.
func Conflict(resource, name string, cause error) error {
	return &domainError{kind: ErrConflict, message: fmt.Sprintf("Operation cannot be fulfilled on %s %q: %v", resource, name, cause), cause: cause}
}

// Invalid is a request refused for what it asks.
func Invalid(message string) error {
	return &domainError{kind: ErrInvalid, message: message}
}

// Wrap makes cause a domain error of kind, its message unchanged.
func Wrap(kind, cause error) error {
	return &domainError{kind: kind, message: cause.Error(), cause: cause}
}
