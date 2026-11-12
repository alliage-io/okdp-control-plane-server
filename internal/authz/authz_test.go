package authz

import (
	"context"
	"errors"
	"testing"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSource is a tree of a platform.yaml and project files.
type fakeSource struct {
	platform string
	projects map[string]string
	err      error
}

func (f fakeSource) PlatformValues(context.Context) ([]byte, error) {
	if f.platform == "" {
		return nil, f.err
	}
	return []byte(f.platform), f.err
}

func (f fakeSource) ProjectFile(_ context.Context, p string) ([]byte, error) {
	if raw, ok := f.projects[p]; ok {
		return []byte(raw), nil
	}
	return nil, f.err
}

func TestTheRolesOfTheCallerComeFromTheProjectsAccess(t *testing.T) {
	a := NewAuthority(fakeSource{
		platform: "global:\n  okdp:\n    access:\n      admins: [platform_admin]\n",
		projects: map[string]string{
			"demo":   "access:\n  analysts: viewer\n  leads: admin\n",
			"broken": "access:\n  analysts: owner\n",
		},
	})
	ctx := context.Background()
	for _, tc := range []struct {
		groups  []string
		project string
		want    Role
	}{
		{[]string{"analysts"}, "demo", compiler.AccessViewer},
		{[]string{"analysts", "leads"}, "demo", compiler.AccessAdmin},
		{[]string{"okdp-project-demo"}, "demo", ""},
		// No project.yaml access: okdp-project-<p> an editor.
		{[]string{"okdp-project-fresh"}, "fresh", compiler.AccessEditor},
		{[]string{"okdp-project-demo"}, "fresh", ""},
		// An access the compile refuses gives nobody a role, but the
		// platform admins.
		{[]string{"analysts"}, "broken", ""},
		{[]string{"platform_admin"}, "broken", compiler.AccessAdmin},
		{[]string{"platform_admin"}, "demo", compiler.AccessAdmin},
		{nil, "demo", ""},
	} {
		role, err := a.Caller(tc.groups).ProjectRole(ctx, tc.project)
		require.NoError(t, err)
		assert.Equal(t, tc.want, role, "%v in %s", tc.groups, tc.project)
	}
}

func TestPermissions(t *testing.T) {
	a := NewAuthority(fakeSource{
		platform: "global:\n  okdp:\n    access:\n      admins: [platform_admin]\n",
		projects: map[string]string{"demo": "access:\n  analysts: viewer\n  devs: editor\n"},
	})
	ctx := context.Background()
	viewer, editor, admin := a.Caller([]string{"analysts"}), a.Caller([]string{"devs"}), a.Caller([]string{"platform_admin"})
	for _, tc := range []struct {
		caller *Caller
		perm   Permission
		want   bool
	}{
		{nil, Public, true},
		{nil, Authenticated, false},
		{viewer, Authenticated, true},
		{viewer, ProjectViewer, true},
		{viewer, ProjectEditor, false},
		{editor, ProjectEditor, true},
		{editor, ProjectAdmin, false},
		{editor, PlatformAdmin, false},
		{admin, ProjectAdmin, true},
		{admin, PlatformAdmin, true},
		{Unrestricted(), PlatformAdmin, true},
		{viewer, Permission(0), false},
	} {
		got, err := tc.caller.Allows(ctx, tc.perm, "demo")
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%v %s", tc.caller.Groups(), tc.perm)
	}
	ok, err := admin.Allows(ctx, ProjectViewer, "")
	require.NoError(t, err)
	assert.False(t, ok, "a project permission without a project")

	assert.NoError(t, Require(WithCaller(ctx, editor), ProjectEditor, "demo"))
	assert.ErrorIs(t, Require(WithCaller(ctx, viewer), ProjectEditor, "demo"), ErrForbidden)
	assert.ErrorIs(t, Require(ctx, ProjectViewer, "demo"), ErrForbidden, "no caller, no right")
}

// A store that cannot answer is an error, never a right.
func TestAnUnreadableAuthorityIsAnError(t *testing.T) {
	a := NewAuthority(fakeSource{err: errors.New("git: unreachable")})
	_, err := a.Caller([]string{"platform_admin"}).Allows(context.Background(), PlatformAdmin, "")
	assert.ErrorContains(t, err, "unreachable")

	// platform.yaml giving admins of another type is refused as the
	// compile refuses it.
	a = NewAuthority(fakeSource{platform: "global:\n  okdp:\n    access:\n      admins: platform_admin\n"})
	_, err = a.Caller([]string{"platform_admin"}).IsPlatformAdmin(context.Background())
	assert.Error(t, err)
}
