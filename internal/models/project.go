package models

// Project represents the simplified domain model
type Project struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CallerAccess is what the caller may do, so the console hides the rest:
// whether it is a platform admin (a group of platform.yaml
// global.okdp.access.admins) and its role in each project it has one in
// (project.yaml access, okdp-project-<p> an editor by default; admin in
// every project for a platform admin). The server refuses the rest with
// 403 {"error", "code": "forbidden"} all the same.
type CallerAccess struct {
	// Username is the caller's, from its token.
	Username string `json:"username"`
	// Groups are the caller's groups, its token's groups claim.
	Groups []string `json:"groups"`
	// PlatformAdmin: a member of a group of global.okdp.access.admins.
	PlatformAdmin bool `json:"platformAdmin"`
	// Projects maps each project the caller has a role in to it: viewer,
	// editor or admin. A project absent has none: the caller does not see
	// it.
	Projects map[string]string `json:"projects"`
}
