package models

// Capabilities describes the optional features the platform is configured
// with, so the UI can adapt (e.g. hide the Identity section when user
// management is not available). Derived from the platform values and the
// server configuration at request time.
type Capabilities struct {
	Identity         IdentityCapability         `json:"identity"`
	OidcProvisioning OidcProvisioningCapability `json:"oidcProvisioning"`
	// Sql are the bounds of the SQL editor.
	Sql SqlCapability `json:"sql"`
}

// SqlCapability are the bounds of the SQL editor (the SQL_* settings), so
// the console bounds its own inputs.
type SqlCapability struct {
	// DefaultRows is the row cap of a request that names none
	// (SQL_DEFAULT_ROWS); MaxRows the largest maxRows a request may ask
	// (SQL_MAX_ROWS), a larger one taken as it.
	DefaultRows int `json:"defaultRows"`
	MaxRows     int `json:"maxRows"`
	// MaxResultBytes caps the size of a result's rows
	// (SQL_MAX_RESULT_BYTES): past it, truncatedBy bytes.
	MaxResultBytes int `json:"maxResultBytes"`
	// StatementTimeoutSeconds bounds one statement (SQL_STATEMENT_TIMEOUT).
	StatementTimeoutSeconds int `json:"statementTimeoutSeconds"`
	// MaxConcurrentPerUser is how many statements one user runs at once
	// (SQL_MAX_CONCURRENT_PER_USER): past it, 429 sql-busy.
	MaxConcurrentPerUser int `json:"maxConcurrentPerUser"`
}

// IdentityCapability describes how the console authenticates against the
// platform's identity provider (Keycloak).
type IdentityCapability struct {
	// Provider is who manages the platform users: "keycloak" when this server
	// manages them through the Keycloak Admin API, "external" otherwise
	// (users are managed in Keycloak directly).
	Provider string `json:"provider"`
	// UserManagement is true when the user/group management API
	// (/api/v1/identity) is available: the server has Keycloak admin
	// credentials and a realm to apply them to.
	UserManagement bool `json:"userManagement"`
	// Oidc is the OIDC client the console UI should authenticate with,
	// resolved from the platform values (global.okdp.identity.oidc). Absent when the platform
	// does not publish it: the UI falls back to its build-time configuration.
	Oidc *IdentityOidcConfig `json:"oidc,omitempty"`
}

// IdentityOidcConfig is the OIDC client configuration served to the console UI.
type IdentityOidcConfig struct {
	// Authority is the OIDC issuer the UI redirects to.
	Authority string `json:"authority"`
	// ClientID is the public (PKCE) client registered for the console.
	ClientID string `json:"clientId"`
	// Scope overrides the UI default scopes when set.
	Scope string `json:"scope,omitempty"`
}

// OidcProvisioningCapability describes the OIDC client provisioning backend.
type OidcProvisioningCapability struct {
	// Provider is the configured provisioning backend: "none" (default) or
	// "keycloak".
	Provider string `json:"provider"`
}
