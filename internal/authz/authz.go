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
	"container/list"
	"context"
	"errors"
	"fmt"
	"github.com/okdp/okdp-gitops/deploy"
	"log/slog"
	"slices"
	"sync"
	"time"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/okdp/okdp-gitops/rules"
	"github.com/okdp/okdp-gitops/treeedit"
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

// Name names the permission in a 403's permission field, for a client:
// platform-admin, viewer, editor or admin (a role of the 403's project);
// "" for public and authenticated, which no 403 names.
func (p Permission) Name() string {
	if p == PlatformAdmin {
		return "platform-admin"
	}
	return string(p.ProjectRole())
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

// projectCacheSize bounds the projects whose access is kept: a project's
// name comes from a URL any authenticated caller writes, the least
// recently read dropped past it.
const projectCacheSize = 4096

// Authority reads the platform's authority from its Source, each part
// kept CacheTTL.
type Authority struct {
	source Source
	ttl    time.Duration

	mu       sync.Mutex
	admins   *entry[[]string]
	projects *accessCache
}

type entry[T any] struct {
	value T
	at    time.Time
}

// accessCache keeps the access of the projects last read, at most max.
// Not safe for concurrent use: Authority.mu guards it.
type accessCache struct {
	max    int
	order  *list.List // of *accessItem, most recently used first
	byName map[string]*list.Element
}

type accessItem struct {
	project string
	entry   *entry[map[string]Role]
}

func newAccessCache(max int) *accessCache {
	return &accessCache{max: max, order: list.New(), byName: map[string]*list.Element{}}
}

func (c *accessCache) get(project string) *entry[map[string]Role] {
	el := c.byName[project]
	if el == nil {
		return nil
	}
	c.order.MoveToFront(el)
	return el.Value.(*accessItem).entry
}

func (c *accessCache) put(project string, e *entry[map[string]Role]) {
	if el := c.byName[project]; el != nil {
		el.Value.(*accessItem).entry = e
		c.order.MoveToFront(el)
		return
	}
	c.byName[project] = c.order.PushFront(&accessItem{project, e})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byName, oldest.Value.(*accessItem).project)
	}
}

func (c *accessCache) forget(project string) {
	if el := c.byName[project]; el != nil {
		c.order.Remove(el)
		delete(c.byName, project)
	}
}

func (c *accessCache) len() int { return c.order.Len() }

// NewAuthority reads the authority of source.
func NewAuthority(source Source) *Authority {
	return &Authority{source: source, ttl: CacheTTL, projects: newAccessCache(projectCacheSize)}
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
	admins, err := compiler.PlatformAdmins(ctx, rules.Standard(), doc)
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
// either. A name no project can have (compiler.IsName) gives no group a
// role, checked first: a URL's :name reaches neither the cache nor the
// log.
func (a *Authority) ProjectAccess(ctx context.Context, project string) (map[string]Role, error) {
	if !compiler.IsName(project) {
		return map[string]Role{}, nil
	}
	a.mu.Lock()
	if e := a.projects.get(project); e != nil && time.Since(e.at) < a.ttl {
		a.mu.Unlock()
		return e.value, nil
	}
	a.mu.Unlock()
	raw, err := a.source.ProjectFile(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", treeedit.ProjectFilePath(project), err)
	}
	access, err := deploy.ProjectAccess(rules.Standard(), raw, project)
	if err != nil {
		slog.Warn("The access of the project cannot be read: no group has a role in it but the platform admins", "error", err, "project", project)
		access = map[string]Role{}
	}
	a.mu.Lock()
	a.projects.put(project, &entry[map[string]Role]{value: access, at: time.Now()})
	a.mu.Unlock()
	return access, nil
}

// Forget drops the access of project kept, so the next read is its
// project.yaml's as it is now: a stream that saw it change reads the
// new roles at once, not within CacheTTL.
func (a *Authority) Forget(project string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.projects.forget(project)
	a.mu.Unlock()
}

// Forget drops the access of project its authority keeps
// (Authority.Forget).
func (c *Caller) Forget(project string) {
	if c != nil {
		c.authority.Forget(project)
	}
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

// Fresh is a caller of the same groups that keeps nothing of c's: what a
// long-lived request reads its rights with, IsPlatformAdmin read anew
// from the authority, not as the request first saw it.
func (c *Caller) Fresh() *Caller {
	if c == nil || c.unrestricted {
		return c
	}
	return &Caller{authority: c.authority, groups: c.groups}
}

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

// ForbiddenError is a caller refused Permission, of Project for a project
// role ("" otherwise): it is ErrForbidden, its message the one a 403
// gives.
type ForbiddenError struct {
	Permission Permission
	Project    string
}

func (e *ForbiddenError) Error() string {
	if e.Permission.ProjectRole() != "" {
		return fmt.Sprintf("this needs the role %s of project '%s'", e.Permission, e.Project)
	}
	return "this needs the role " + e.Permission.String()
}

func (e *ForbiddenError) Is(target error) bool { return target == ErrForbidden }

// Forbidden is the refusal of perm, of project for a project role.
func Forbidden(perm Permission, project string) *ForbiddenError {
	if perm.ProjectRole() == "" {
		project = ""
	}
	return &ForbiddenError{Permission: perm, Project: project}
}

// Require is nil when the caller of ctx holds perm in project, a
// *ForbiddenError otherwise (or the error of reading the authority).
func Require(ctx context.Context, perm Permission, project string) error {
	ok, err := CallerFrom(ctx).Allows(ctx, perm, project)
	if err != nil {
		return err
	}
	if !ok {
		return Forbidden(perm, project)
	}
	return nil
}

type routeKey struct{}

type route struct {
	perm    Permission
	project string
}

// WithRoute returns ctx carrying the permission of its route and the
// project it is of (the router's table, middleware.Authorize):
// RecheckRoute reads them.
func WithRoute(ctx context.Context, perm Permission, project string) context.Context {
	return context.WithValue(ctx, routeKey{}, route{perm, project})
}

// RecheckRoute reads anew whether the caller of ctx still holds the
// permission of its route: the context to go on with, its caller Fresh, or
// a *ForbiddenError, or the error of reading the authority. A stream's
// periodic check that its request is still allowed, the caller's rights
// read from the authority as it is now, not as at the request's start;
// nil for a context without a route (no middleware.Authorize).
func RecheckRoute(ctx context.Context) (context.Context, error) {
	r, ok := ctx.Value(routeKey{}).(route)
	if !ok {
		return ctx, nil
	}
	caller := CallerFrom(ctx).Fresh()
	ctx = WithCaller(ctx, caller)
	allowed, err := caller.Allows(ctx, r.perm, r.project)
	if err != nil {
		return ctx, err
	}
	if !allowed {
		return ctx, Forbidden(r.perm, r.project)
	}
	return ctx, nil
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
