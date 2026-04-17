package models

// User represents an identity user for API responses/requests
type User struct {
	Name     string   `json:"name"`     // spec.name (Display Name / Full Name)
	Username string   `json:"username"` // metadata.name (ID / Login)
	Email    []string `json:"email,omitempty"`
	Comment  string   `json:"comment,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
	UID      int      `json:"uid,omitempty"`
	// Groups is a computed field, not directly in the User CRD but useful for API
	Groups []string `json:"groups,omitempty"`
	// Password is write-only, used for creation/update
	Password string `json:"password,omitempty"`
	// Internal: used to preserve hash during update if password not changed
	PasswordHash string `json:"-"`
}

// Group represents an identity group for API responses/requests
type Group struct {
	Name        string `json:"name"`
	Comment     string `json:"comment,omitempty"`
	Description string `json:"description,omitempty"` // Alias for Comment if needed
}

// GroupBinding represents the link between a user and a group
type GroupBinding struct {
	User  string `json:"user"`
	Group string `json:"group"`
}
