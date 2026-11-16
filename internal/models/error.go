package models

// ErrorResponse is the body of every error answer of the API (4xx, 5xx):
// error always, the other fields when they apply, so a client reads one
// shape whatever the route.
type ErrorResponse struct {
	// Error is what went wrong, for a person. A failure of the server
	// itself (5xx) says so without its cause, which stays in the server's
	// log under RequestID.
	Error string `json:"error"`
	// Code tells a client what kind of refusal it is, when the answer has
	// one: forbidden, does-not-compile, instance-exists,
	// release-name-taken, sql-busy, sql-timeout...
	Code string `json:"code,omitempty"`
	// Fields are the problems of a compile at the fields they are about
	// (code does-not-compile, or a connection or claim the compiler's
	// rules refuse), for a form that shows each beside its field.
	Fields []FieldProblem `json:"fields,omitempty"`
	// RequestID is the request's id (the X-Request-Id header), on a
	// failure of the server, to quote to its administrator.
	RequestID string `json:"requestId,omitempty"`
	// Permission is, on a 403 code forbidden, the permission the route
	// needs: platform-admin, viewer, editor or admin (a role of Project).
	Permission string `json:"permission,omitempty" enums:"platform-admin,viewer,editor,admin"`
	// Project is, on a 403 for a project role, the project it is a role
	// of.
	Project string `json:"project,omitempty"`
}

// FieldProblem is a problem of a compile at one field of an instance's
// parameters (compiler.FieldErrors).
type FieldProblem struct {
	// Path is the field as the compile's messages name it, keys joined by
	// dots, an index in brackets: catalogs[1].storage, claims.analytics;
	// "" for the instance as a whole.
	Path string `json:"path"`
	// Pointer is the same field as a JSON pointer (RFC 6901):
	// /catalogs/1/storage; "" for the instance as a whole.
	Pointer string `json:"pointer"`
	// Message is the compile's.
	Message string `json:"message"`
	// Instance is the instance the problem is of, <project>/<instance>,
	// when a compile of the tree refused the write: the instance written
	// or another one the write breaks (one that uses what it changes).
	// Absent when the refusal is of no instance (the tree, a project's
	// name) or the problem is of what the request itself declares (a
	// connection, a claim's provider).
	Instance string `json:"instance,omitempty"`
}
