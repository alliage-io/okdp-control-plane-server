// Package authz decides what a caller of the API may do, from the
// platform's own authority (decision D-1): the groups of the platform's
// administrators, platform.yaml global.okdp.access.admins, and the roles
// of each project's groups, its project.yaml access (okdp-project-<p> an
// editor by default). The compiler reads both (compiler.PlatformAdmins,
// compiler.ProjectAccess) and writes the same roles as Argo CD AppProject
// roles and namespace RoleBindings; Forgejo maps the same groups to its
// teams. The control plane only mirrors that authority: it adds no
// boundary only it enforces, and grants nothing the platform does not.
package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-compiler/treeedit"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/sirupsen/logrus"
)

// Role is the role of a group within a project: compiler.AccessViewer,
// AccessEditor or AccessAdmin, each what the one before it may do, and
// more.
type Role = compiler.AccessRole

// rank orders the roles, none first.
func rank(r Role) int {
	switch r {
	case compiler.AccessViewer:
		return 1
	case compiler.AccessEditor:
		return 2
	case compiler.AccessAdmin:
		return 3
	}
	return 0
}

// AtLeast reports whether r grants what want grants.
func AtLeast(r, want Role) bool { return rank(r) >= rank(want) && rank(want) > 0 }

// Permission is what a route needs (the router's table).
type Permission int

const (
	// Public: no token (the console's bootstrap, /api/capabilities).
	Public Permission = iota + 1
	// Authenticated: any caller with a verified token.
	Authenticated
	// PlatformAdmin: a member of a group of global.okdp.access.admins.
	PlatformAdmin
	// ProjectViewer, ProjectEditor, ProjectAdmin: that role, or more, in
	// the project of the route's :name; a platform admin has every role.
	ProjectViewer
	ProjectEditor
	ProjectAdmin
)

// String names the permission in a refusal.
func (p Permission) String() string {
	switch p {
	case Public:
		return "public"
	case Authenticated:
		return "authenticated"
	case PlatformAdmin:
		return "platform admin"
	case ProjectViewer:
		return "project viewer"
	case ProjectEditor:
		return "project editor"
	case ProjectAdmin:
		return "project admin"
	}
	return "none"
}

// ProjectRole is the role a project permission needs, "" for another.
func (p Permission) ProjectRole() Role {
	switch p {
	case ProjectViewer:
		return compiler.AccessViewer
	case ProjectEditor:
		return compiler.AccessEditor
	case ProjectAdmin:
		return compiler.AccessAdmin
	}
	return ""
}

// Source is the tree the authority is read from.
type Source interface {
	// PlatformValues is platform.yaml as written, nil without it.
	PlatformValues(ctx context.Context) ([]byte, error)
	// ProjectFile is projects/<project>/project.yaml as written, nil
	// without it.
	ProjectFile(ctx context.Context, project string) ([]byte, error)
}

// DeploymentsSource reads the authority from the deployments repositories,
// where the compile reads it.
func DeploymentsSource(d *gitops.Deployments) Source { return deploymentsSource{d} }

type deploymentsSource struct{ d *gitops.Deployments }

func (s deploymentsSource) PlatformValues(ctx context.Context) ([]byte, error) {
	raw, err := s.d.ReadPlatformValues(ctx)
	if errors.Is(err, treeedit.ErrNotFound) {
		return nil, nil
	}
	return raw, err
}

func (s deploymentsSource) ProjectFile(ctx context.Context, project string) ([]byte, error) {
	if !compiler.IsName(project) {
		return nil, nil
	}
	var raw []byte
	err := s.d.Store.View(ctx, func(r treeedit.Reader) error {
		b, err := r.ReadFile(treeedit.ProjectFilePath(project))
		if errors.Is(err, treeedit.ErrNotFound) {
			return nil
		}
		raw = b
		return err
	})
	return raw, err
}

// CacheTTL bounds how stale the authority may be: a group added to
// global.okdp.access.admins or to a project's access counts within it.
const CacheTTL = 5 * time.Second

// Authority reads the platform's authority from its Source, each part
// kept CacheTTL.
type Authority struct {
	source Source
	ttl    time.Duration

	mu       sync.Mutex
	admins   *entry[[]string]
	projects map[string]*entry[map[string]Role]
}

type entry[T any] struct {
	value T
	at    time.Time
}

// NewAuthority reads the authority of source.
func NewAuthority(source Source) *Authority {
	return &Authority{source: source, ttl: CacheTTL, projects: map[string]*entry[map[string]Role]{}}
}

// PlatformAdmins are the groups of global.okdp.access.admins, as the
// compile reads them (compiler.PlatformAdmins); none without platform.yaml.
func (a *Authority) PlatformAdmins(ctx context.Context) ([]string, error) {
	a.mu.Lock()
	if e := a.admins; e != nil && time.Since(e.at) < a.ttl {
		a.mu.Unlock()
		return e.value, nil
	}
	a.mu.Unlock()
	raw, err := a.source.PlatformValues(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", compiler.PlatformFile, err)
	}
	doc := map[string]any{}
	if raw != nil {
		if doc, err = treeedit.DecodeValues(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", compiler.PlatformFile, err)
		}
	}
	admins, err := compiler.PlatformAdmins(doc)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.admins = &entry[[]string]{value: admins, at: time.Now()}
	a.mu.Unlock()
	return admins, nil
}

// ProjectAccess is the roles of the groups of project, as the compile
// reads them (compiler.ProjectAccess): okdp-project-<project> an editor
// when its project.yaml gives no access. A file the compile refuses gives
// no group a role (its error logged): the platform does not apply it
// either.
func (a *Authority) ProjectAccess(ctx context.Context, project string) (map[string]Role, error) {
	a.mu.Lock()
	if e := a.projects[project]; e != nil && time.Since(e.at) < a.ttl {
		a.mu.Unlock()
		return e.value, nil
	}
	a.mu.Unlock()
	raw, err := a.source.ProjectFile(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", treeedit.ProjectFilePath(project), err)
	}
	access, err := compiler.ProjectAccess(raw, project)
	if err != nil {
		logrus.WithError(err).WithField("project", project).Warn("The access of the project cannot be read: no group has a role in it but the platform admins")
		access = map[string]Role{}
	}
	a.mu.Lock()
	a.projects[project] = &entry[map[string]Role]{value: access, at: time.Now()}
	a.mu.Unlock()
	return access, nil
}

// Caller is what one caller may do: the authority of its groups, read on
// demand and kept for the request.
type Caller struct {
	authority *Authority
	groups    []string
	// unrestricted: authentication is off (AUTH_DISABLED), every right.
	unrestricted bool

	mu    sync.Mutex
	admin *bool
}

// Caller is the caller of groups, the verified token's.
func (a *Authority) Caller(groups []string) *Caller {
	return &Caller{authority: a, groups: groups}
}

// Unrestricted is the caller of an API served without authentication
// (AUTH_DISABLED, local development only): every right.
func Unrestricted() *Caller { return &Caller{unrestricted: true} }

// Groups are the caller's groups.
func (c *Caller) Groups() []string {
	if c == nil {
		return nil
	}
	return c.groups
}

// IsPlatformAdmin reports whether a group of the caller is one of
// global.okdp.access.admins.
func (c *Caller) IsPlatformAdmin(ctx context.Context) (bool, error) {
	if c == nil {
		return false, nil
	}
	if c.unrestricted {
		return true, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.admin != nil {
		return *c.admin, nil
	}
	if c.authority == nil {
		return false, nil
	}
	admins, err := c.authority.PlatformAdmins(ctx)
	if err != nil {
		return false, err
	}
	is := slices.ContainsFunc(c.groups, func(g string) bool { return slices.Contains(admins, g) })
	c.admin = &is
	return is, nil
}

// ProjectRole is the caller's role in project: the highest of its groups'
// in the project's access, admin for a platform admin, "" for none.
func (c *Caller) ProjectRole(ctx context.Context, project string) (Role, error) {
	if c == nil {
		return "", nil
	}
	admin, err := c.IsPlatformAdmin(ctx)
	if err != nil {
		return "", err
	}
	if admin {
		return compiler.AccessAdmin, nil
	}
	if c.authority == nil {
		return "", nil
	}
	access, err := c.authority.ProjectAccess(ctx, project)
	if err != nil {
		return "", err
	}
	var best Role
	for _, g := range c.groups {
		if r, ok := access[g]; ok && rank(r) > rank(best) {
			best = r
		}
	}
	return best, nil
}

// Allows reports whether the caller holds perm, project the route's
// project for a project permission.
func (c *Caller) Allows(ctx context.Context, perm Permission, project string) (bool, error) {
	switch perm {
	case Public:
		return true, nil
	case Authenticated:
		return c != nil, nil
	case PlatformAdmin:
		return c.IsPlatformAdmin(ctx)
	case ProjectViewer, ProjectEditor, ProjectAdmin:
		if project == "" {
			return false, nil
		}
		role, err := c.ProjectRole(ctx, project)
		return AtLeast(role, perm.ProjectRole()), err
	}
	return false, nil
}

// ErrForbidden is a caller refused a right.
var ErrForbidden = errors.New("forbidden")

// Require is nil when the caller of ctx holds perm in project, an error
// wrapping ErrForbidden otherwise (or the error of reading the
// authority).
func Require(ctx context.Context, perm Permission, project string) error {
	ok, err := CallerFrom(ctx).Allows(ctx, perm, project)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: this needs the role %s of project %s", ErrForbidden, perm, project)
	}
	return nil
}

type callerKey struct{}

// WithCaller returns ctx carrying the caller.
func WithCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom is the caller of ctx, nil without one (no right but Public).
func CallerFrom(ctx context.Context) *Caller {
	c, _ := ctx.Value(callerKey{}).(*Caller)
	return c
}
