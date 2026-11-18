package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/okdp/okdp-gitops/gitops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
)

// handWritten is a project a GitOps user declared in Git: the console never
// saw it, and its namespace (created by the engine) has no okdp.io/project
// label. Its description is the key description of project.yaml.
const handWritten = "# declared by hand\ndescription: Demo project\nglobal: {}\n"

func newProjectServiceUnderTest(files map[string]string) (*DefaultProjectService, *mocks.ProjectNamespaceRepository, *gitops.MemoryStore) {
	namespaces := new(mocks.ProjectNamespaceRepository)
	store := gitops.MustMemoryStore(files)
	return NewDefaultProjectService(namespaces, gitops.NewDeployments(store)), namespaces, store
}

// Requirement 3: a project written by hand in Git is a project for the console.
func TestProjectsWrittenInGitAreListedAndResolved(t *testing.T) {
	svc, _, _ := newProjectServiceUnderTest(map[string]string{
		"projects/demo/project.yaml":                 handWritten,
		"projects/dcr/project.yaml":                  "{}\n",
		"projects/dcr/instances/trino/instance.yaml": "service: trino\nversion: 1.0.0\n",
		"projects/no-file/instances/a/instance.yaml": "service: trino\nversion: 1.0.0\n",
		"platform.yaml":                              "{}\n",
	})
	ctx := context.Background()

	projects, err := svc.ListProjects(ctx)
	require.NoError(t, err)
	assert.Equal(t, []models.Project{{Name: "dcr"}, {Name: "demo", Description: "Demo project"}}, projects)

	project, err := svc.GetProject(ctx, "demo")
	require.NoError(t, err)
	assert.Equal(t, &models.Project{Name: "demo", Description: "Demo project"}, project)

	for _, name := range []string{"no-file", "kube-system", "Bad_Name"} {
		_, err = svc.GetProject(ctx, name)
		assert.True(t, errors.Is(err, apperr.ErrNotFound), "%s: got %v", name, err)
	}
}

func TestListProjectsIsEmptyNotNull(t *testing.T) {
	svc, _, _ := newProjectServiceUnderTest(nil)
	projects, err := svc.ListProjects(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, projects)
	assert.Empty(t, projects)
}

func TestCreateProjectCommitsProjectYamlAndCreatesTheNamespace(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(nil)
	ctx := aliceContext()
	project := &models.Project{Name: "demo", Description: "Demo project"}

	namespaces.On("CreateNamespace", ctx, project).Return(true, nil)
	require.NoError(t, svc.CreateProject(ctx, project))
	assert.Equal(t, "description: Demo project\n", store.Files()["projects/demo/project.yaml"])
	assertNoOutput(t, store.Files())
	assert.Equal(t, "okdp: create project demo by alice"+coAuthor, store.Messages[len(store.Messages)-1])
	namespaces.AssertExpectations(t)
}

func TestCreateProjectRefusesAProjectAlreadyInGit(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	ctx := context.Background()
	project := &models.Project{Name: "demo"}

	// The engine created the namespace of the hand-written project: it has
	// no label, so the namespace answers first.
	namespaces.On("CreateNamespace", ctx, project).Return(false, apperr.AlreadyExists("namespaces", "demo")).Once()
	err := svc.CreateProject(ctx, project)
	assert.True(t, errors.Is(err, apperr.ErrAlreadyExists), "got %v", err)

	// A namespace the console left behind is reused: Git answers.
	namespaces.On("CreateNamespace", ctx, project).Return(false, nil).Once()
	err = svc.CreateProject(ctx, project)
	assert.True(t, errors.Is(err, apperr.ErrAlreadyExists), "got %v", err)

	assert.Equal(t, handWritten, store.Files()["projects/demo/project.yaml"], "untouched")
	namespaces.AssertExpectations(t)
}

func TestCreateProjectRemovesTheNamespaceWhenGitRefuses(t *testing.T) {
	svc, namespaces, _ := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	ctx := context.Background()
	project := &models.Project{Name: "demo"}

	namespaces.On("CreateNamespace", ctx, project).Return(true, nil)
	namespaces.On("DeleteNamespace", mock.Anything, "demo").Return(true, nil)
	err := svc.CreateProject(ctx, project)
	assert.True(t, errors.Is(err, apperr.ErrAlreadyExists), "got %v", err)
	namespaces.AssertExpectations(t)
}

// The namespace of a project Git refused is removed even when the client
// is gone by then (SRV-25): the rollback's context is not the request's.
func TestCreateProjectRemovesTheNamespaceWhenTheClientIsGone(t *testing.T) {
	svc, namespaces, _ := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	ctx, cancel := context.WithCancel(context.Background())
	project := &models.Project{Name: "demo"}

	namespaces.On("CreateNamespace", mock.Anything, project).Run(func(mock.Arguments) { cancel() }).Return(true, nil)
	namespaces.On("DeleteNamespace", mock.MatchedBy(func(c context.Context) bool {
		_, hasDeadline := c.Deadline()
		return c.Err() == nil && hasDeadline
	}), "demo").Return(true, nil)
	err := svc.CreateProject(ctx, project)
	require.Error(t, err)
	namespaces.AssertExpectations(t)
}

func TestCreateProjectRefusesAnInvalidName(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(nil)
	err := svc.CreateProject(context.Background(), &models.Project{Name: "Not_A_Label"})
	assert.True(t, errors.Is(err, apperr.ErrInvalid), "got %v", err)
	assert.Empty(t, store.Messages)
	namespaces.AssertNotCalled(t, "CreateNamespace")
}

// A name the compile refuses, kube-* or one of platform.yaml's
// global.okdp.policy.reserved, is a BadRequest of the compiler's message,
// before any Namespace or commit.
func TestCreateProjectRefusesAReservedName(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(map[string]string{
		"platform.yaml": "global:\n  okdp:\n    policy:\n      reserved: [openbao]\n",
	})
	for name, want := range map[string]string{
		"kube-foo": `project "kube-foo": the namespace is reserved: kube-* is Kubernetes' own (kube-system, kube-public, kube-node-lease)`,
		"argocd":   `project "argocd": the namespace is reserved: a deployer engine's, or the namespace its target writes in (argocd, flux-system, okdp-releases): a project's releases there would drive the engine`,
		"openbao":  `project "openbao": the namespace is reserved by the platform (global.okdp.policy.reserved): no project has it`,
	} {
		err := svc.CreateProject(aliceContext(), &models.Project{Name: name})
		assert.True(t, errors.Is(err, apperr.ErrInvalid), "%s: got %v", name, err)
		assert.EqualError(t, err, want)
	}
	assert.Empty(t, store.Messages)
	namespaces.AssertNotCalled(t, "CreateNamespace")
}

func TestCreateProjectStopsOnANamespaceError(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(nil)
	ctx := context.Background()
	project := &models.Project{Name: "demo"}

	namespaces.On("CreateNamespace", ctx, project).Return(false, errors.New("forbidden"))
	assert.EqualError(t, svc.CreateProject(ctx, project), "forbidden")
	assert.Empty(t, store.Messages)
}

func TestUpdateProjectWritesTheDescriptionOfAHandWrittenProject(t *testing.T) {
	svc, _, store := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	ctx := context.Background()

	updated, err := svc.UpdateProject(ctx, &models.Project{Name: "demo", Description: "Edited"})
	require.NoError(t, err)
	assert.Equal(t, &models.Project{Name: "demo", Description: "Edited"}, updated)
	assert.Equal(t, "# declared by hand\ndescription: Edited\nglobal: {}\n", store.Files()["projects/demo/project.yaml"])

	_, err = svc.UpdateProject(ctx, &models.Project{Name: "missing", Description: "x"})
	assert.True(t, errors.Is(err, apperr.ErrNotFound), "got %v", err)
}

// Deleting removes projects/<p>/ from Git, so the engine uninstalls every
// release, then the namespace if the console created it.
func TestDeleteProjectRemovesItFromGitThenTheNamespace(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(map[string]string{
		"projects/demo/project.yaml":                 handWritten,
		"projects/demo/instances/hive/instance.yaml": "service: hive-metastore\nversion: 1.0.0\n",
	})
	ctx := context.Background()

	namespaces.On("DeleteNamespace", ctx, "demo").Return(false, errors.New("the cluster is away")).Once()
	assert.NoError(t, svc.DeleteProject(ctx, "demo"), "gone from Git is what counts")
	for p := range store.Files() {
		assert.NotContains(t, p, "projects/demo/")
	}

	err := svc.DeleteProject(ctx, "demo")
	assert.True(t, errors.Is(err, apperr.ErrNotFound), "got %v", err)
	namespaces.AssertExpectations(t)
}

func TestWatchProjectsStreamsTheGitChanges(t *testing.T) {
	svc, namespaces, _ := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	svc.PollInterval = time.Hour // only the console's own changes wake the stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := svc.WatchProjects(ctx)
	require.NoError(t, err)
	next := func() ProjectEvent {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
			return ProjectEvent{}
		}
	}
	assert.Equal(t, ProjectEvent{Type: "ADDED", Object: models.Project{Name: "demo", Description: "Demo project"}}, next())

	namespaces.On("CreateNamespace", ctx, &models.Project{Name: "sales"}).Return(true, nil)
	require.NoError(t, svc.CreateProject(ctx, &models.Project{Name: "sales"}))
	assert.Equal(t, ProjectEvent{Type: "ADDED", Object: models.Project{Name: "sales"}}, next())

	_, err = svc.UpdateProject(ctx, &models.Project{Name: "demo", Description: "Edited"})
	require.NoError(t, err)
	assert.Equal(t, ProjectEvent{Type: "MODIFIED", Object: models.Project{Name: "demo", Description: "Edited"}}, next())

	namespaces.On("DeleteNamespace", ctx, "sales").Return(true, nil)
	require.NoError(t, svc.DeleteProject(ctx, "sales"))
	assert.Equal(t, ProjectEvent{Type: "DELETED", Object: models.Project{Name: "sales"}}, next())

	cancel()
	for range events {
	}
}

// A change of a project's access alone, by hand in Git, is MODIFIED: the
// console reads the caller's rights (GET /api/me) again on it.
func TestWatchProjectsStreamsAChangeOfTheAccessAlone(t *testing.T) {
	svc, _, store := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	svc.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := svc.WatchProjects(ctx)
	require.NoError(t, err)
	next := func() ProjectEvent {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
			return ProjectEvent{}
		}
	}
	demo := models.Project{Name: "demo", Description: "Demo project"}
	assert.Equal(t, ProjectEvent{Type: "ADDED", Object: demo}, next())

	write := func(content string) {
		t.Helper()
		_, err := store.Update(ctx, gitops.Commit{Subject: "access"}, func(tx gitops.Tx) error {
			return tx.WriteFile("projects/demo/project.yaml", []byte(content))
		})
		require.NoError(t, err)
	}
	write(handWritten + "access:\n  analysts: viewer\n")
	assert.Equal(t, ProjectEvent{Type: "MODIFIED", Object: demo}, next())

	// A change of another key of project.yaml is no change of the project.
	write(handWritten + "access:\n  analysts: viewer\n# a comment\n")
	select {
	case e := <-events:
		t.Fatalf("unexpected event %+v", e)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	for range events {
	}
}

func TestProjectErrorMapsAnExistingRepositoryToAConflict(t *testing.T) {
	err := projectError(fmt.Errorf("owner/demo: %w", gitops.ErrRepositoryNotEmpty), "demo")
	assert.True(t, errors.Is(err, apperr.ErrConflict), "got %v", err)
	assert.False(t, errors.Is(err, apperr.ErrAlreadyExists), "got %v", err)
	assert.Contains(t, err.Error(), "not empty")
}
