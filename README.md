<p align="center">
  <a href="https://okdp.io">
    <img src="https://okdp.io/logos/okdp-inverted.png" alt="OKDP: Open Kubernetes Data Platform" height="180" />
  </a>
</p>

[![release-please](https://github.com/OKDP/okdp-control-plane-server/actions/workflows/release-please.yml/badge.svg)](https://github.com/OKDP/okdp-control-plane-server/actions/workflows/release-please.yml)
[![License Apache2](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](http://www.apache.org/licenses/LICENSE-2.0)

# OKDP Control Plane Server

Docker image and Helm chart for the backend API of the OKDP web console
([okdp-control-plane-ui](https://github.com/OKDP/okdp-control-plane-ui)): a Go service that
manages OKDP projects and their data services on Kubernetes and exposes them to the console
over REST and Server-Sent Events (SSE).

---

## Why this project

The OKDP web console is a browser single-page application with no cluster credentials and
no server-side logic, so it cannot drive Kubernetes on its own. This repository is its
backend: an API that turns console actions into commits to the sources of the platform's
deployments Git repositories (projects, service instances, connections, the service
catalog), each instance checked by a compile with
[okdp-compiler](https://github.com/OKDP/okdp-compiler) from a package of the platform's
git catalogs, and reads back what the cluster reports, streaming status, pods and
metrics, and shows the values each chart of an instance is rendered with, compared with
its defaults. The output the engines deploy is not the console's to write:
[okdp-gitops](https://github.com/OKDP/okdp-gitops) writes it, in the CI of the output
repository, which compiles the latest commits of the sources on a schedule.

It is a first-party OKDP component, developed alongside the web console. Cluster credentials
and business logic stay on the server side, keeping the console a stateless browser client.

---

## What the project does

- **Docker image** `quay.io/okdp/images/okdp-control-plane-server`: runs the Go control-plane API
  server on a minimal Alpine base (`alpine:3.24`, built with `golang:1.27.3`, both pinned
  by digest in the `Dockerfile` and bumped by Dependabot), listening on port `8093`.
- **Helm chart** (`chart/`): deploys the server in-cluster with its Git settings and its
  RBAC outside the projects' namespaces; in each project's namespace, its rights are the
  compiled output's ([RBAC](#rbac)).

The image and chart provide the backend for the OKDP web console, so the platform is
driven through the console instead of hand-written GitOps files. Both paths produce the
same files: a console deployment and a hand-written one are indistinguishable.

Project layout:

- `cmd/server`: entry point.
- `internal/api`: HTTP handlers and router.
- `internal/service`: business logic.
- `internal/repository`: cluster and repository access (platform values, what the instances publish, GitOps engine status, the workloads). The Kubernetes clients stay here, one `rest.Config`, their errors translated to domain errors.
- `internal/apperr`: the domain errors (`ErrNotFound`, `ErrAlreadyExists`, `ErrConflict`, `ErrInvalid`), translated to a status by `handlers.writeError`.
- `internal/config`: configuration loaded from environment variables.
- `chart/`: Helm chart.

The reads and the edits of the tree, with their checks by the compile of the tree, are
okdp-compiler's package `treeedit` (its types and errors: `treeedit.Instance`,
`treeedit.Project`, `treeedit.CompileError`, `treeedit.ErrInUse`, `treeedit.ErrNotFound`...).
The deployments repositories (topology, go-git writer, the Git transactions that commit
each edit, `gitops.Deployments`), the compile of the tree with okdp-compiler (each
instance's outcome, the claims bound to each provider) and the catalogs of packages are
the packages `gitops` and `catalog` of [okdp-gitops](https://github.com/OKDP/okdp-gitops),
a module of its own (`go.mod` replaces it with `../okdp-gitops`, a checkout next to this
one, until it is published); the chart cache is okdp-compiler's `chartcache`.

---

## Architecture

<p align="center">
  <img src="docs/assets/architecture.svg" alt="OKDP Control Plane Server topology" />
</p>

> **OKDP deployment context:** the server runs **in-cluster**. **Git is the only
> desired-state store**: the server never creates HelmReleases, Applications or workloads;
> it commits the tree to the deployments repositories of the sources, the CI of the output
> repository, running `okdp-gitops compile` on a schedule, writes the output in a
> repository of its own, and a GitOps engine (FluxCD or
> Argo CD) deploys each instance from its compiled directory in the output repository, the
> same under both engines. What an instance publishes (its URL, its usage, the connections
> it provides) is read from its compiled `okdp.info.yaml`; observed state from the
> cluster: the workloads (`app.kubernetes.io/instance`) and the engine's own objects for
> render/sync errors. There is no database; the local clones are a cache. Authentication
> (OIDC) is verified on every `/api` call, and each route's permission against the
> platform's own authority ([authorization](#authorization)); TLS is provided by the
> platform ingress.

The tree is okdp-compiler's
([repository layout](https://github.com/OKDP/okdp-compiler/blob/main/docs/01_usage.md#repository-layout)),
its parts each in the repository the topology says (below):

```
platform.yaml                                           # the platform: global.okdp, the classes of the claims, the platform's connections
catalogs.yaml                                           # the catalogs (repositories) and the console menu (treeedit.CatalogsFile)
projects.yaml                                           # split topology: the projects (treeedit.ProjectsFile)
projects/<project>/project.yaml                         # declares the project: its globals, classes, connections, named claims
projects/<project>/instances/<instance>/instance.yaml   # {service, version}: the package
projects/<project>/instances/<instance>/config.yaml     # the submitted parameters only (none without parameters)
compiled/projects/<project>/instances/<project>-<instance>/
                                                        # okdp-compiler's umbrella chart (Chart.yaml, instance.values.yaml,
                                                        # okdp.info.yaml), helmrelease.yaml,
                                                        # kustomization.yaml (Flux), application.yaml (Argo CD)
sources.yaml                                            # the output: the revisions of the sources and catalogs it is the compile of
```

`platform.yaml`, `catalogs.yaml` and `projects/` hold what people and the console write
(the tree); `compiled/` (the output) is generated from them by okdp-gitops, the files of
okdp-compiler's Flux and Argo CD targets, by the CI of the output repository (every few
minutes, when the tree moved), with `sources.yaml`, the revisions of the sources and
catalogs it is the compile of, and is the only thing the engines read. The console never
writes it: what it reads there (what an instance publishes, its compiled values) is what
the last run wrote, and lags behind the tree for the time of a run. The platform's own services (the database servers, the
ingress, the control plane) are instances of projects too: there are no platform
components, and the deployment graph (connection references, claims) is the only order.

### Authorization

The console is a mirror of the platform's authority, never an authority of its own: the
control plane is optional, every boundary holds without it, and the server grants no right
the platform does not, nor refuses one only it would enforce. Its authority is the one
okdp-compiler reads (decision D-1), from the tree:

- the **platform admins**, the groups of `platform.yaml` `global.okdp.access.admins`
  (`compiler.PlatformAdmins`), which the platform's bootstrap makes Argo CD and Forgejo
  admins;
- the **roles of each project**, `project.yaml` `access:` mapping groups to `viewer`,
  `editor` or `admin`, `okdp-project-<project>` an editor when it gives none
  (`compiler.ProjectAccess`, `compiler.DefaultProjectGroup`), which the compiler writes as
  the project's Argo CD AppProject roles and its namespace's RoleBindings to `view`, `edit`
  and `admin`, and Forgejo maps to its teams.

The caller's groups are its verified token's groups claim (`OIDC_GROUPS_CLAIM`, `groups`
by default; chart `oidc.groupsClaim`): the groups' names as Keycloak emits them, its groups
mapper's "Full group path" off (`platform_admin`, not `/platform_admin`). Every route has a
permission in the router's table (`router.Permissions`), deny by default: a route missing
from it answers `403` to everyone.

| Permission | Routes |
|---|---|
| none (public) | `GET /api/capabilities`, the console's bootstrap |
| authenticated | `GET /api/me`; `GET /api/projects` and `/api/projects/stream` (the projects the caller may see only); the catalog and the platform's settings: `GET /api/platform-services[/…]`, `/api/platform-categories`, `/api/contracts`, `/api/profile-images`, `/api/spark-config`, `/api/spark-app-schema` |
| platform admin | `/api/v1/identity/*` (users and groups, every method); `POST`, `PUT`, `DELETE /api/platform-services[/…]` (the menu of `catalogs.yaml`); `POST /api/projects`, `DELETE /api/projects/:name` |
| project viewer | `GET /api/projects/:name` and every `GET` and stream under `/api/projects/:name/`; `POST …/services/:s/sql` (Trino, the caller's token exchanged for the engine's audience) |
| project editor | every write under `/api/projects/:name/` (instances, connections and their tests, named claims, secret stores and their tests, external secrets, Spark applications); `POST …/connections/:c/sql`, and the write mode of the SQL editor |
| project admin | `PUT /api/projects/:name` (its `project.yaml`) |

A stream (`/api/projects/stream`, `.../services/stream`, `.../spark-apps/stream`, a log
followed) re-checks its route's permission at each keepalive (30s, the authority read as it
is then) and ends when its token expires, with an SSE `error` event naming why; the console
reconnects, a new request. A stream also ends, without an event, when the server stops: at
`SIGTERM` the streams end at once, the other requests in flight are served to their end
(a commit and its Secret are written together), within 45s, the chart's
`terminationGracePeriodSeconds` (60s) above it. Each write of a stream has its own 15s
deadline: a client that stops reading loses the stream.

A platform admin has every project role. A caller without the permission gets `403`
`{"error": "<what it needs>", "code": "forbidden", "permission": "<permission>",
"project": "<project>"}`: `permission` is `platform-admin`, `viewer`, `editor` or `admin`,
`project` the project of a project role (absent for `platform-admin`). The write mode of
the SQL editor refused is a `403` code `sql-write-refused` with the same `permission` and
`project`. `GET /api/me` answers what the caller may do, so the console hides the rest,
and the bounds of the SQL editor (`sql`, an authenticated caller's: the public
`GET /api/capabilities` no longer carries them):

```json
{"username": "alice", "groups": ["okdp-project-demo"], "platformAdmin": false, "projects": {"demo": "editor"},
 "sql": {"defaultRows": 1000, "maxRows": 10000, "maxResultBytes": 16777216, "statementTimeoutSeconds": 120, "maxConcurrentPerUser": 2}}
```

The authority is read from the deployments repositories, kept 5 seconds: a group added to
`access.admins` or to a project's `access` counts within that time. A `project.yaml`
whose `access` the compile refuses gives no group a role in that project but the platform
admins. With `AUTH_DISABLED` (local development only), every caller holds every right.
The console cannot change `access:` (the project's `PUT` edits its description only): it
is changed in Git, where the project's admins push.

### Topology: where the parts live

`GITOPS_CONFIG`, required, names a topology file (the chart's `gitops.topology`):

```yaml
platform: {url: <git url>, branch: main, path: ""}   # the directory of platform.yaml, catalogs.yaml
projects: {url: <git url, may contain {project}>, branch: main, path: "projects/{project}"}
output:   {url: <git url>, branch: main, path: ""}   # the directory of compiled/ (and argocd/root.yaml)
forgejo:                                             # optional: creates the repository of a new project
  url: http://forgejo-http.forgejo.svc.cluster.local:3000
  owner: okdp
  tokenFile: /etc/okdp/forgejo/token
```

The output is always in a repository of its own, which the CI alone writes: never the
platform's nor a project's, on no branch (the server refuses such a topology at startup).
The sources are in one input repository (`io`: the platform and the projects,
`projects.path` `projects/{project}`; the platform best in a repository of its own, for
the platform admins), or each project in a repository of its own (`split`:
`projects.url` names `{project}`; the projects are listed in `projects.yaml`, beside
`platform.yaml`, `projects: [{name: demo}]`, each entry optionally overriding `url`,
`branch` or `path`). Without `{project}` in its url, `projects.path` ends with
`/{project}`: the projects are the directories of its parent. There is no
single-repository mode: `GITOPS_REPO_URL`, `GITOPS_BRANCH` and `GITOPS_PATH` are gone, and
a server still given `GITOPS_REPO_URL` refuses to start; their repository is the
topology's `platform` and `projects` (`path: <GITOPS_PATH>/projects/{project}`), its output
moved to a repository of its own.

A console change in one repository is one commit. Across repositories it is one commit per
repository, with the same message: the projects' repositories, then the platform's (a
repository that moved meanwhile gets the same change on its new revision). The console
writes nothing to the output repository: its CI compiles the sources at its next run. In
a split topology, creating a project creates its repository in the `forgejo` owner, named
after the topology's projects url (`okdp-project-{project}.git`: `okdp-project-<project>`),
through the Forgejo API (the token of `tokenFile`), pushes its `project.yaml` only (no workflow, no
secret: the output's CI reads it with its own token), then adds it to `projects.yaml`;
without a `forgejo` section that creation fails with a message saying so. Deleting it
removes its entry (the next compile, its compiled tree); its repository is kept.

Projects are the `projects/<project>/project.yaml` files (the namespace is the project
name), whoever wrote them: a project declared by hand in Git is listed, served and edited
by the console like one the console created (in a split topology, the projects of
`projects.yaml`), and `/api/projects/stream` reports the files as they change (read every
few seconds): a change of a project's `access` alone, as the compile reads it, is a
`MODIFIED` too, the caller's role in it read anew, a project it starts seeing `ADDED` and
one it stops seeing `DELETED`; the console reads `GET /api/me` again on any event. A
project's description is the key `description` of its `project.yaml`,
the project's own (okdp-compiler leaves it out of the context of the project's instances);
an update rewrites that key and keeps the rest of the file. Creating a project from the
console refuses first, 400 with the compiler's message and nothing created, a name the
compile would refuse every instance of the project with (okdp-compiler's
`compiler.CheckProjectName`, through `treeedit.CheckProjectName` and okdp-gitops'
`Deployments.CheckProjectName`):
`platform`, `kube-*`, the deployer engines' namespaces (`argocd`, `flux-system`,
`okdp-releases`) and those of `platform.yaml`'s `global.okdp.policy.reserved`. It also
creates its Namespace (label `okdp.io/project`, and the Pod Security Admission labels
of okdp-compiler's namespace manifest: `baseline` enforced, `restricted` audited and
warned of) so it can hold Secrets before any deployment, and refuses a name that is already a Namespace without that label. Deleting
a project removes
`projects/<project>/` (the compile of that commit removes its compiled tree, and the
engine uninstalls its releases), refused while an instance of another project uses one of
its instances, then the Namespace only if it carries the label; a namespace the engine
created is left, as when the directory is removed by hand.

Commits read `okdp: <action> <project>/<name> by <user>`. The logged-in user is the
commit author; the configured GitOps identity (`GITOPS_AUTHOR_NAME` / `GITOPS_AUTHOR_EMAIL`)
is the committer, and the server names itself as co-author in a trailer:

```
Author:    Alice Martin <alice@example.com>
Committer: OKDP control plane <okdp-control-plane@okdp.io>

    okdp: deploy demo/trino by alice

    Co-Authored-By: okdp-control-plane-server v0.9.0 <okdp-control-plane@okdp.io>
```

The trailer names the program (the last element of the Go module path) and its version
(the server sets okdp-gitops' `gitops.Program` to its own), set at build time
(`-ldflags "-X github.com/okdp/okdp-control-plane-server/internal/buildinfo.Version=<v>"`:
the image build passes the release version as the `VERSION` build argument, `make build`
passes `git describe`; `dev` otherwise), with the committer email. The author comes from
the access token: the `email` claim (Keycloak: the `email` client scope on the console
client) and the `name` claim (the `profile` scope; the user name when absent). Without an
email (the claim is missing or not an address, or authentication is disabled) the GitOps
identity authors the commit too, the user is named in the subject only, the message has
no trailer, and the server logs a warning once per user. CR, LF, `<` and `>` are stripped
from the name and email. Writes are serialised and
replayed on top of the new revision when someone else pushed in between. Reads are not:
each reads a snapshot of the last commit of each repository it reaches, one revision of
each for the whole read, immutable, taken without a lock (okdp-gitops); no read waits on
another, on a compile or on a write, and a stale snapshot (5 s) is fetched again by one
read while the others read the one it replaces. A request of several reads may see a
newer revision in a later one.

### Checking a write

Every write is checked, inside the write, by a compile of the whole tree it leaves
(okdp-gitops: `compiler.Build` of the tree and the catalogs), compared with the compile
of the tree before it, the verdict okdp-compiler's (`treeedit.Check.Verify`): deploying or updating an
instance, changing a connection, a named claim or a project. It is refused, nothing
committed, when the instance it writes does not compile, when an instance that compiled
before does not any more (a class, a named claim or a connection of a project reaches
every instance of it, a claim binds across projects), or when the tree no longer loads;
the API answers `400` with the compiler's message (code `does-not-compile`) and its
problems at their fields, `fields: [{path, pointer, message, instance}]`
(`compiler.FieldErrors`: `catalogs[1].storage`, `/catalogs/1/storage`; `""` for the
instance as a whole), `instance` the `<project>/<instance>` the compile refused, the one
written or another one the write breaks (absent when the tree as a whole is refused). The server
re-implements none of the compiler's rules: the parameters, the connection references and
the claims of an instance are the compile's to check, the keys the context gives
(`global`, `connections`, `claims`) and a version that is not in the catalogs among them,
a menu entry of `catalogs.yaml` (its service a package of the catalogs, its default one of
its versions) the compile's check of the file (okdp-gitops `Compiler.CheckCatalogs`, a
`400 does-not-compile` too), a connection at its declaration
`compiler.CheckConnection`'s, the contracts `compiler.ReadContracts`', the connection
references of a package `compiler.ConnectionRefs`', the chart defaults under the compiled
values `compiler.CoalesceValues`'. Removing an
instance, a connection, a named claim or a project is refused (`400`, naming them) while
instances use it: they would not compile without it. The compiled files are not committed:
the CI of the output compiles the commit again at its next run, the whole tree, and
writes them (below, [the output](#the-output-okdp-gitops)), so a commit made by hand and a console one are
compiled the same way.

The server's compiles (these checks and the reads of the tree) do not render the upstream
charts (`gitops.Compiler.SkipRender`, `service.NewTreeCompiler`): Helm's engine takes no
context, so a render past its timeout would leave its goroutine in the server, and its
memory is bounded only once it returns. What a project's upstream values deploy is checked
by the render of the CI's compile, the boundary: a write only the render refuses is
committed, then the CI's compile refuses the instance and says so. A chart that could not be
fetched refuses no write either.

An instance names a package: `service` and `version` in `instance.yaml`, the directory
`catalog/<service>/<version>/` of a catalog (below). [okdp-compiler](https://github.com/OKDP/okdp-compiler)
compiles it under the release `<project>-<instance>` in the namespace `<project>`, with the
context of `platform.yaml` (level 0), its project's `project.yaml` (level 1: its globals,
its classes, its connections, its named claims) and its own `connections.yaml` (level 2,
rare), its values `config.yaml`, the contracts of the catalogs, what the other instances
publish, each claim of a connection reference bound to the instance that provisions its
contract, of any namespace. The parameters are checked against the package's
`config.schema.json`, the connections against their contracts, and the values of each
upstream chart computed; the instance is written as one chart, an umbrella over its
upstream charts and the chart of OKDP's own objects (`Chart.yaml`,
`instance.values.yaml`, what it publishes in `okdp.info.yaml`). Each connection reference to
another instance (an output it publishes, by its name, or a claim it provisions) becomes a
`dependsOn` of the HelmRelease and a later Argo CD sync wave.

The server compiles with a chart cache: it pulls each chart the umbrellas name, from its
own repository (an OCI registry or an https chart repository, anonymously), into its chart
cache (`GITOPS_CHART_CACHE_DIR`, laid out as okdp-compiler reads it,
`<repository>/<chart>-<version>/<chart>/`), and compiles again with it (the chart's
`values.schema.json`, the lists an instance appends to). A chart that cannot be fetched is
logged and the tree compiled without the checks: never a failure. The compile of the same
tree and catalog revisions is kept, for the reads that follow; the reads that ask for it
at once share one compile. The same cache gives the
chart defaults of the rendered values view (`GET .../services/:serviceName/values`: each
chart's defaults coalesced under its compiled values, the lines set by OKDP or the
instance marked).

### Connections

A project's connections are the entries `connections.<name>` of its `project.yaml`:
`contract`, `description`, the contract's fields, `secretRef: {name: <secret>}`, the
Secret of the project namespace holding its credentials, which the console writes
(`<name>-credentials`) or that was already there (projected from a vault). The platform's
connections are the entries of `platform.yaml` (an S3 store or a database the platform
uses and does not deploy, their credentials from the platform's secret store,
`secretRef: {remoteKey: <key>}`): every instance may reference them, the deployment forms
offer them (`GET .../connections/selectable`, scope `platform`), and the platform's
administrators write them in Git; a project connection may not take the name of one. An
instance references a connection by its name, in its parameters; nothing else lists it.
A connection the console writes is checked at its declaration as the compile checks it
(`compiler.CheckConnection`: its contract, its fields, none unknown nor secret, its
required ones after completion, their types, its `secretRef`), whether an instance
references it yet or not; the credentials it requires (`x-okdp-required` of a secret field)
are the console's to ask, the compile never sees them.

A write of a connection and its Secret is not atomic, so the commit comes first: a create or
an update commits the connection naming its Secret, then writes the Secret (refused before
the commit when a Secret the console did not write holds the name); credentials it cannot
store take the commit back (the connection deleted, or restored as it was). A crash in
between leaves a connection whose credentials are not stored yet (an update writes them),
never a Secret that no connection names. A delete, or an update off the console's Secret,
commits first and removes the Secret next: a crash in between leaves the Secret, labelled
`app.kubernetes.io/managed-by: okdp-control-plane`, which a new connection of that name
takes over (the server's Role in a project namespace cannot list Secrets, so no sweep finds
it).

### Secret stores

A project's SecretStores (`/api/projects/:name/secret-stores`, External Secrets'
`external-secrets.io/v1`) name a Vault (OpenBao) server. One of Kubernetes auth makes
External Secrets mint a token of a ServiceAccount of the namespace and send it to that
server, so the server pins it: the ServiceAccount is the platform's secret store's in the
namespace, as the compile writes it (`global.okdp.secrets.serviceAccount`, `okdp-secrets`
by default, which the compiled manifests of each project create), a request naming
another is refused; the token's `audiences` are the platform's provider's
(`global.okdp.secrets.provider.vault.auth.kubernetes.serviceAccountRef.audiences`, the
sandbox's `openbao`), never the API server's; the server is the platform's store's
(`global.okdp.secrets.provider.vault.server`) or one of `secretStores.allowedServers`
(`SECRET_STORE_ALLOWED_SERVERS`). Without the platform's provider, or with no audiences,
Kubernetes auth is refused (`400`). The connection test (`POST .../secret-stores/test`)
reaches an allowed server only, through the SQL editor's guard (loopback, link-local and
the API server refused unless `sqlEditor.allowedNetworks`), follows no redirect, and
reports a failure to connect without its cause, which the server logs.

### SQL editor

`POST /api/projects/<p>/services/<instance>/sql` runs a statement on a Trino instance with
a token of the caller exchanged for Trino's audience at the issuer (RFC 8693 token exchange
by the Keycloak client of `KEYCLOAK_CLIENT_ID`, audience `SQL_TOKEN_EXCHANGE_AUDIENCE`), never
the caller's own, which the API would accept back from whoever controls the engine: Trino
authorizes the exchanged token. Without an exchange configured the statement answers
`400`, code `sql-no-token-exchange`, nothing forwarded; `502`, code `sql-token-exchange`,
when the identity provider refuses it, or gives a token the API would accept (naming the
console's client in `aud`, `azp` or `client_id`), or an opaque one. The realm must
allow the exchange: Keycloak 26.2 or later, standard token exchange enabled on the
exchange client (`standard.token.exchange.enabled`, the attribute of the client, distinct
from the console's own), the caller's access token carrying that client in its `aud`
(an audience mapper on the console's client, what Keycloak requires of the subject
token), the requested audience (Trino's client) available to the exchange client through a
client scope (an audience mapper), and Trino accepting that audience (its OAuth client,
or `http-server.authentication.oauth2.additional-audiences`, of each instance). `POST /api/projects/<p>/connections/<c>/sql`
runs one on a `database-server` connection (engine `postgresql`) declared in the tree: the
project's, else the platform's, else a named claim of the project (the connection its
provider granted it). Host, port and database are the connection's, never the request's,
and the credentials come from four sources only:

- the claim's provisioned Secret, labelled `okdp.io/credentials-database-server` by
  okdp-compiler;
- the platform's secret store (`secretRef: {remoteKey: <key>}`): the Secret an
  ExternalSecret of the project fills from that key, once an instance references it;
- the Secret the console wrote from the connection form (`<name>-credentials`);
- a Secret named by `secretRef.name` (the console's `existingSecret`) carrying the label
  `okdp.io/connection-credentials: "true"`, which its owner sets to allow it.

Any other Secret is refused (`400`, code `sql-credentials-refused`): naming an operator's
superuser Secret in a connection is not a way to hand it to every member of the project.
The connection verifies TLS against the system's CAs and the platform's CA bundle
(`tls.caBundleSecret`, [TLS](#tls): `sslMode: verify-ca` checks the chain only, any other mode the
host name too, `disable` and `prefer` are not honored), accepts SCRAM-SHA-256 only (a
server asking for a clear-text or MD5 password gets nothing), and dials through a guard
refusing loopback, link-local (`169.254.0.0/16`, `fe80::/10`) and the Kubernetes API
server unless `sqlEditor.allowedNetworks` allows them; connection tests (`POST
.../connections/test`) go through the same guard, with the credentials the caller typed
only, and report a generic category, never the remote server's text.

A statement runs read-only by default: alone (a batch of several is an SQL error), in a
`READ ONLY` transaction rolled back after it. `write: true` in the request runs a batch of
statements in write mode, a project `editor`'s ([authorization](#authorization)); a statement
on a connection, read-only or not, is an editor's too (it runs with credentials of the
project's Secrets, which a viewer cannot read), one on a Trino service a viewer's (with a
token exchanged for the engine, never the caller's own). Read-only guards against
mistakes; the boundary is what the database grants the connection's user. Every statement is logged with the caller, the connection and the
start of its text, and is bounded by `SQL_STATEMENT_TIMEOUT`, a row cap, a result-size
cap and a per-user concurrency limit (`429`, code `sql-busy`); a client that disconnects
cancels it. A result carries the row cap it ran with (`maxRows`: the request's, else
`SQL_DEFAULT_ROWS`, at most `SQL_MAX_ROWS`) and, when `truncated`, which cap dropped
rows (`truncatedBy`: `rows` or `bytes`); `GET /api/me` gives the bounds
(`sql: {defaultRows, maxRows, maxResultBytes, statementTimeoutSeconds,
maxConcurrentPerUser}`). A PostgreSQL statement past the timeout answers `200` with
`error.errorName: TIMEOUT`; a Trino one `504`, code `sql-timeout`, the query cancelled.
`GET .../connections/selectable` and `GET .../claims` give each connection's `engine`
and whether the editor runs on it (`sql`); any other target answers `400`, code
`sql-unsupported`. An instance that publishes no URL yet answers `400`, code
`sql-no-url`, a connection or a claim whose credentials Secret does not exist yet `400`,
code `sql-no-credentials`.

### Spark applications

`POST /api/projects/<p>/spark-apps` builds a SparkApplication from an allow-list, never
from a manifest passed through: its driver and executors run as the ServiceAccount of the
project's `spark-rbac` instance (its compiled `serviceAccount.name`; a project without one
is refused), its image is one the platform offers (`global.okdp.sparkOperator`:
`spark.images`, or under `image.registry`; any when neither is set), its `sparkConf` keys
are Spark's own settings: no `spark.kubernetes.*` key but the pods' labels, resources and
executor allocation, nothing naming volumes, a ServiceAccount, a pod template, a host path
or a security context. `POST .../spark-apps/yaml` reads a manifest into the same request
(plus `imagePullPolicy`, `restartPolicy`, `deps`, `coreLimit`) and refuses any other
field; an update is checked alike and runs as the project's ServiceAccount.

### Claims

A connection reference of a package (`x-okdp-connection-ref` with `provision: true`) may
claim its connection instead of naming one: a database, a bucket of its own, which the
instance that provisions the contract creates and publishes, under the claim's ID
(`<project>.<release>.<parameter>`, `<project>.<name>` for a named claim), its credentials
copied into the claimer's namespace through the platform's secret store
(`global.okdp.secrets.store`) when the provider is of another one
([okdp-compiler: claims](https://github.com/OKDP/okdp-compiler/blob/main/docs/specs/01_inputs_outputs.md#claims)):

| Value of the parameter | Means |
|---|---|
| `{}` (a package's default) | a connection of its own, of the default class of the contract |
| `{class: <class>}`, `{provider: <namespace>/<instance>}` | of that class, of that provider |
| `{request: {...}}`, with any | what the claim requests, as the contract's `x-okdp-claim` allows |
| `<name>` | a connection, a named claim of the project, or an output of an instance of the project |

The classes are the platform's and the projects' (`global.okdp.provisioning.<contract>`:
`default`, and `classes.<name>: {provider, request}`, in `platform.yaml` and in a project's
`project.yaml`, a provider named `<namespace>/<instance>` or by its release); a project's named claims are its
`project.yaml` `claims.<name>: {contract, class, provider, request}`, one connection for
every instance of the project that names it.

No connection is computed from a name: a provider publishes its connections
(`okdp.info.yaml`), an instance's own output named after its release by default, and the
connection forms list the outputs of the project's instances by their names
(`GET .../connections/selectable`, `.../internal`), but the connections a provider
publishes for its claims, named after the claims' IDs (dotted), which are their
claimers'. `GET /api/contracts` gives each contract's claim schema (`claimSchema`, the
contract's `x-okdp-claim`): what a claim may request.

| Route | |
|---|---|
| `GET /api/projects/:name/claims` | the project's named claims, each with the provider the compile binds it to, the instances that name it, and why it does not bind |
| `POST /api/projects/:name/claims`, `PUT`, `DELETE .../claims/:claimName` | declare, change, remove a named claim in `project.yaml` (its comments kept): its name and the form of its provider checked (`compiler.CheckProvider`), the rest by the compile of the tree (the contract, the class or the provider, the request against `x-okdp-claim` and the class), as the project's instances bind it: refused when an instance that compiled stops compiling; a project without instance binds none, so none of its named claims is checked until one is deployed; a named claim an instance names is not removed |
| `GET /api/projects/:name/provisioning` | the classes of each contract's claims, the platform's and the project's, the default class, the claim schema |
| `GET /api/projects/:name/services/:serviceName/claims` | the claims of the instance's references, each bound and what its provider granted (`okdp.info.yaml`); for a provider (its package's `provisions`), the claims bound to it, their claimers and what it granted each |

Deploying or updating an instance has the claims its parameters write or name checked by
the compile of the write: the keys of a claim (`class`, `provider`, `request`), its class
(of the project or the platform), its request (the package's fixed one, the instance's
merged over it) against the contract's claim schema and what the class fixes, each problem
at the field of its reference. The deployment
form reads, for each connection reference (`GET .../inputs`), whether it takes a claim
(`provision`), the package's fixed `request`, what it requires of the connection
(`require`) and its default claim (`defaultClaim`).

### Packages and catalogs

A package is a directory of a git catalog, in okdp-compiler's format
([writing a package](https://github.com/OKDP/okdp-compiler/blob/main/docs/02_writing_package.md)):

```
catalog/<service>/<version>/   Chart.yaml (name, version, the upstream charts as dependencies),
                               okdp.yaml, values.cel.yaml or values.yaml, config.schema.json (the
                               parameters), <key>.defaults.yaml
contracts/<contract>.schema.json
```

`catalogs.yaml`, beside `platform.yaml`, names the catalogs and the console menu, read
strictly as okdp-compiler reads it (`treeedit.ParseCatalogs`): any other key, a former
`serviceCatalog` wrapper or a service's `versions` or `repository`, is refused:

```yaml
repositories:
  - {name: platform-charts, url: https://github.com/OKDP/platform-charts.git, ref: main}
  - {name: sandbox-dependencies, url: https://github.com/OKDP/sandbox-dependencies.git, ref: v1.0.0}
categories:
  - title: Query Engine
    icon: pi-bolt
    services:
      - {name: trino, description: Trino}       # optional default: <version>, else the highest
```

Each catalog is fetched at its `ref` (a branch, a tag or a commit; `main` by default) and
read again every 30 seconds; a catalog on the host of the deployments repositories is read
with their credentials, any other anonymously. A service's versions are its directories
`catalog/<service>/<version>/`, highest first; a service name is unique across the
catalogs, and a contract two catalogs hold must be the same bytes in both (else an error
naming both). The console's deployment form is the package's `config.schema.json`; the
contracts of the connection forms are the catalogs'.

### The output: `okdp-gitops`

[okdp-gitops](https://github.com/OKDP/okdp-gitops) writes the output: the CI of the
output repository runs `okdp-gitops compile` every few minutes, compiles the latest
commits of the tree, the console's and those made by hand alike, and commits what changed
to the output repository (`okdp: compile`), which the engines reconcile; `sources.yaml`,
at the root of the output, records the revisions of the sources and catalogs it is the
compile of (a run whose sources did not move stops early). `compiled/` holds the files of
okdp-compiler's targets as they write them ([targets](https://github.com/OKDP/okdp-compiler/blob/main/docs/specs/01_inputs_outputs.md#targets)):
per instance, `compiled/projects/<project>/instances/<project>-<instance>/`, its umbrella
chart, its Flux HelmRelease (`okdp-releases/<project>-<instance>`, built from the output's
`GitRepository` `flux-system/okdp-gitops`, `dependsOn` the releases it waits for) and the
kustomization of its values ConfigMap, and its Argo CD Application
(`argocd/<project>-<instance>`, its sync wave after those it waits for) when the output
holds `argocd/root.yaml` (an Argo CD Application whose `spec.source` is the repository the
engine reads the output from); per project, a Flux kustomization; and what the engines
need to sync the whole tree. The server reads it back: what an instance publishes
(`okdp.info.yaml`, with the package of the `Chart.yaml` beside it), its compiled values,
the labels of the engine objects (`okdp.io/project`, `okdp.io/instance`). From a checkout
of okdp-gitops (next to one of okdp-compiler):

```sh
make build
bin/okdp-gitops compile --tree <sources> --output <output>          # write what changed
bin/okdp-gitops compile --tree <sources> --output <output> --check  # fail when stale
bin/okdp-gitops compile --config topology.yaml --catalog platform-charts=../platform-charts
bin/okdp-gitops workflow --for output --config topology.yaml        # the CI workflow of the output repository
```

`--tree DIR --output DIR` is the io topology of two local directories, the platform and
the projects in the first, the output in the other; `--config FILE` a topology file, whose
urls are git repositories (cloned; the changes committed and pushed as `okdp: compile`,
with the credentials of `$GITOPS_CREDENTIALS_DIR`, else the token `$GITOPS_TOKEN`) or
local directories (an absolute path, or one relative to the file: read and written in
place). `--catalog NAME=DIR` reads the catalog `NAME` of `catalogs.yaml` from a local checkout
instead of its url; `--charts DIR` checks the values against the upstream charts, pulled
into that chart cache, as the server checks them. Every problem of the tree is reported at
once, and nothing is written: the engines keep deploying the last output. `--path-prefix`
is the path of the output in its Git repository, for the paths the engines read (default:
the topology's output path).

The output repository alone holds a workflow, `.forgejo/workflows/okdp-gitops.yaml`
(Forgejo Actions, `okdp-gitops workflow --for output`), run on a schedule on a runner
dedicated to okdp-gitops, with the only token of the CI (the output repository's secret
`OKDP_GITOPS_TOKEN`, its user the only writer of the output, a reader of the sources).
The repositories of the sources hold no workflow and no token: the server creates the
repository of a new project without either. The runner it needs is described in
[okdp-gitops' README](https://github.com/OKDP/okdp-gitops#the-ci-of-the-output).

---

## Requirements

- Kubernetes cluster reconciled by [FluxCD](https://fluxcd.io) (helm-controller,
  source-controller, kustomize-controller) or [Argo CD](https://argo-cd.readthedocs.io),
  from the output repository of the deployments above
- The CI of the output repository running [okdp-gitops](https://github.com/OKDP/okdp-gitops)
  on a schedule (Forgejo Actions, `.forgejo/workflows/okdp-gitops.yaml`): it writes the
  output
- The catalogs of packages (git repositories) the server can fetch, and the upstream
  charts' repositories (for the checks and the chart defaults)
- Write access to the deployments repositories of the sources, read access to the
  output's (HTTPS token or SSH key)
- [Go](https://go.dev/) >= 1.27 (only to build the image or run the server locally)

Known-good baseline: chart and image `0.10.0` <!-- x-release-please-version -->
with Go `1.27`, on a Kind cluster. This is the version set validated by the maintainers.

### Toolchain tested

| Tool | Version |
|---|---|
| Go | `1.27` |
| Kind | `0.27.0` |
| kubectl | `1.32.2` |

> The server does not need the `helm` or `kubectl` binaries to run: it communicates with the
> Kubernetes API through the client-go library. Helm (>= 3) is needed only to deploy the
> chart, and `kubectl` to inspect the cluster.

---

## Installation

The server runs in-cluster, next to a GitOps engine reconciling the output repository.
Give it write access to the repositories of the sources (read access to the output's)
through a Secret using the Flux `GitRepository` keys:

```sh
kubectl create secret generic okdp-gitops-credentials -n okdp-system \
  --from-literal=username=okdp-console --from-literal=password=<token>
# or, over SSH: --from-file=identity=./id_ed25519 --from-file=known_hosts=./known_hosts
```

The platform values (`global.okdp`) are read from `platform.yaml` in Git, the context
every instance is compiled with (the server's check, the CI's).

Install the chart from the OKDP registry, with the topology of the repositories
(`gitops.topology`, required, written to a ConfigMap mounted at
`/etc/okdp/gitops/topology.yaml`, the server's `GITOPS_CONFIG`):

```yaml
# values.yaml: an io topology, the sources in one repository, the output in another
gitops:
  topology:
    platform: {url: https://git.example.com/okdp/okdp-sources.git}
    projects: {url: https://git.example.com/okdp/okdp-sources.git, path: "projects/{project}"}
    output:   {url: https://git.example.com/okdp/okdp-output.git}
  credentialsSecret: okdp-gitops-credentials
  engine: flux
```

<!-- x-release-please-start-version -->
```sh
helm install okdp-control-plane-server oci://quay.io/okdp/charts/okdp-control-plane-server --version 0.10.0 \
  -n okdp-system --create-namespace -f values.yaml
```
<!-- x-release-please-end -->

For a split topology (a repository per project), add `gitops.topology.forgejo` (`url`,
`owner`) and `gitops.forgejoTokenSecret` (a Secret with the key `token`) for the server to
create the repository of a new project. The chart's former `gitops.repoURL`,
`gitops.branch` and `gitops.path` are gone: their repository, branch and path are the
topology's `platform` (`path: <path>`) and `projects` (`path: <path>/projects/{project}`),
the output moved to a repository of its own.

> This chart name is published on Quay once the rename lands upstream. Until
> then, install from `chart/` in this checkout.

Once the pod is `Running`, reach the API through a port-forward:

```sh
kubectl port-forward -n okdp-system svc/okdp-control-plane-server 8093:8093
curl -s http://localhost:8093/health
# {"status":"ok"}
```

The API is described by [`docs/swagger.yaml`](docs/swagger.yaml) (`make swagger`); the
server does not serve it. Every answer carries an `X-Request-Id` header; an internal error
answers a generic message with that `requestId`, its cause in the server's log under it.
Every error answer is a `models.ErrorResponse`, `{error, code?, fields?, requestId?,
permission?, project?}` (a `501` of a feature not installed `{error, reason, feature}`),
so a client types it from the spec.

### RBAC

The chart grants the server only what lies outside the projects' namespaces:

| Where | What |
|---|---|
| cluster | `namespaces`: `get`, `create`, `delete` (a project's Namespace), with a ValidatingAdmissionPolicy (`rbac.namespacePolicy`, Kubernetes >= 1.30) letting its ServiceAccount create only a Namespace labelled `okdp.io/project` with its name and a Pod Security level that is not `privileged`, and delete only a Namespace so labelled |
| cluster | the CRD `sparkapplications.sparkoperator.k8s.io`: `get` (the SparkApplication form) |
| `gitops.releasesNamespace` (flux) or `gitops.argocdNamespace` (argocd) | `helmreleases` or `applications`: `get`, `list`, `watch` (the render/sync status) |

In each project's namespace the rights are the compiled output's: okdp-compiler writes
there the Role `okdp-control-plane` and its RoleBinding to the ServiceAccount
`global.okdp.controlPlane.serviceAccount` of `platform.yaml` names: `secrets` `get`,
`create`, `update`, `delete` (by name, never listed), `pods` `get`, `list`, `delete`,
`pods/log` `get`, `events` `list`, `persistentvolumeclaims` `get`, `list`, `delete`,
`metrics.k8s.io` `pods` `list`, `external-secrets.io` `secretstores` and
`externalsecrets` `get`, `list`, `create`, `update`, `delete`, `sparkoperator.k8s.io`
`sparkapplications` `get`, `list`, `watch`, `create`, `update`, `delete`. So `platform.yaml`
must name the chart's ServiceAccount, `<release>-okdp-control-plane-server` in the
release's namespace (the release's name alone when it contains
`okdp-control-plane-server`; `helm install` prints it):

```yaml
# platform.yaml, for the release okdp-control-plane-server in okdp-system
global:
  okdp:
    controlPlane:
      serviceAccount: {namespace: okdp-system, name: okdp-control-plane-server}
```

Without it, the server reads and writes nothing in the projects' namespaces (`403` from
the Kubernetes API, answered `500`). A project the console creates gets its Namespace at
once, but the Role only when the engine applies the project's compiled manifests (the
output's next run, then the sync): until then its Secrets, secret stores and Spark
applications cannot be written. Every claim's credentials Secret the SQL editor reads
is in the namespace of its provider's project, which holds the Role too.

---

## Cleanup

Remove the Helm release, and the namespace if it was created only for this install:

```sh
helm uninstall okdp-control-plane-server -n okdp-system
kubectl delete namespace okdp-system
```

---

## Development

### Run the server locally

Both options require a `KUBECONFIG` pointing at a Kubernetes cluster (the server
connects to the Kubernetes API at startup) and a topology file naming the deployments
repositories: any Git URL works, local bare repositories included (`git init --bare
/tmp/okdp-sources.git /tmp/okdp-output.git`):

```yaml
# /tmp/topology.yaml
platform: {url: file:///tmp/okdp-sources.git}
projects: {url: file:///tmp/okdp-sources.git, path: "projects/{project}"}
output:   {url: file:///tmp/okdp-output.git}
```

**On your machine.** Install Go and the development tools (`kubectl`, `air`,
`swag`, `golangci-lint`, `delve`), then:

```sh
export KUBECONFIG=<path-to-your-kubeconfig>
export GITOPS_CONFIG=/tmp/topology.yaml
make dev                 # hot-reload on :8093
# or, without hot-reload:
go run ./cmd/server
```

**In the devcontainer.** Only Docker is required: open the repository and select "Reopen
in Container" (or run `devcontainer up`). The container ships the full toolchain,
publishes port 8093 and syncs a container-reachable copy of the host kubeconfig at
startup. Then run `make dev`.

### Build, test and lint

No cluster is required, but okdp-gitops checked out next to this repository (`go.mod`
replaces it with `../okdp-gitops` until it is published; CI and the image build fetch it
there at the commit `OKDP_GITOPS_REF` of their workflows, and the image copies the module
whole from the build context). okdp-compiler and cel2yaml are downloaded at the releases
`go.mod` requires:

```sh
make build     # compile the binary to bin/server
make test      # run the tests
make lint      # run golangci-lint
make swagger   # regenerate the Swagger documentation
docker build --build-context okdp-gitops=../okdp-gitops -t okdp-control-plane-server .
```

Run `make help` for the full list of targets.

---

## Configuration

The server is configured through environment variables, rendered by the chart from
its `configuration:` values (see [`chart/values.yaml`](chart/values.yaml)).

| Parameter | Description | Default | Required |
|-----------|-------------|---------|:--------:|
| `PORT` | HTTP port the server listens on | `8093` | No |
| `ALLOWED_ORIGINS` | Single CORS origin, set verbatim in `Access-Control-Allow-Origin` (the console URL) | `http://localhost:4200` | No |
| `LOG_LEVEL` | Log verbosity (`debug`, `info`, `warn`, `error`) | `info` | No |
| `TRUSTED_PROXIES` | Peers (addresses or CIDRs, comma-separated; chart: `configuration.trustedProxies`) whose `X-Forwarded-For` is believed for the client address of the audit log: the ingress controller's pods. None by default, the client is then the address of the connection | | No |
| `GIN_MODE` | HTTP framework mode (`debug` prints every route) | `release` | No |
| `OIDC_ISSUER` / `OIDC_CLIENT_ID` | Override the issuer and the client the console's tokens are verified against | the platform values (`global.okdp.identity.oidc`, else `global.okdp.oidc`) | No |
| `OIDC_GROUPS_CLAIM` | The token's claim of the caller's groups, a list of group names, a dotted name for a nested claim (chart: `oidc.groupsClaim`); the permissions are read with them ([authorization](#authorization)) | `groups` | No |
| `AUTH_DISABLED` | `true` serves the API without verifying tokens, every caller holding every right (local development only) | | No |
| `GITOPS_CONFIG` | Topology file: the repositories of the platform, the projects and the output (`https://…`, `ssh://…`, `git@host:path`, `file://…`), and Forgejo ([topology](#topology-where-the-parts-live)). `GITOPS_REPO_URL`, `GITOPS_BRANCH` and `GITOPS_PATH` (one repository holding the whole tree) are gone; a `GITOPS_REPO_URL` still set is refused at startup | | Yes |
| `GITOPS_CREDENTIALS_DIR` | Mounted credentials Secret (`username`/`password`, `bearerToken`, or `identity`/`known_hosts`), for every deployments repository, and the catalogs on their host | `/etc/okdp/gitops-credentials` | No |
| `GITOPS_SSH_INSECURE_IGNORE_HOST_KEY` | Accept any SSH host key (sandboxes only) | `false` | No |
| `GITOPS_INSECURE_HTTP_HOSTS` | Hosts a credential (the repositories', the catalogs', the Forgejo API token) is sent to over `http://`, in clear: comma-separated host names or globs (`path.Match`) of them (chart: `gitops.insecureHTTPHosts`). Name the exact hosts (a sandbox's `forgejo-http.forgejo.svc.cluster.local`); without it, every credential for an `http://` URL is refused and the server answers the fetch, the commit or the repository creation with that error | | No |
| `GITOPS_AUTHOR_NAME` / `GITOPS_AUTHOR_EMAIL` | Commit committer (and author when the user has no email; the logged-in user authors the commit otherwise) | `OKDP control plane` / `okdp-control-plane@okdp.io` | No |
| `GITOPS_CLONE_DIR` | Local clones, a cache, one per repository under `repos/` (empty: in memory) | in memory | No |
| `GITOPS_CHART_CACHE_DIR` | Chart cache: the upstream charts of the compiled instances | `<GITOPS_CLONE_DIR>/charts`, else under the temporary directory | No |
| `GITOPS_ENGINE` | `flux` or `argocd`: whose objects carry render/sync errors | `flux` | No |
| `GITOPS_RELEASES_NAMESPACE` | Flux HelmReleases and their values ConfigMaps | `okdp-releases` | No |
| `ARGOCD_NAMESPACE` | Argo CD Applications | `argocd` | No |
| `INSECURE_OCI_REGISTRIES` | Registry hosts reached over plain HTTP to pull the upstream charts into the chart cache (sandboxes) | | No |
| `GITOPS_PRIVATE_CHART_HOSTS` | Chart hosts the chart cache may reach at a private address, an in-cluster registry (chart: `configuration.privateChartHosts`); the cache trusts `OKDP_CA_FILE` beside the system CAs | | No |
| `KEYCLOAK_CLIENT_ID` | Keycloak service-account client of the platform realm (chart: `keycloak.credentialsSecret`, key `client_id`) | `okdp-control-plane` | No |
| `KEYCLOAK_CLIENT_SECRET` | Its secret (chart: `keycloak.credentialsSecret`, key `client_secret`). Unset: user and group management is off | | No |
| `KEYCLOAK_URL` / `KEYCLOAK_REALM` | Keycloak base URL and realm of the Admin API | from the platform OIDC issuer `<url>/realms/<realm>` | No |
| `KEYCLOAK_TLS_INSECURE` | Skip the Keycloak certificate check (discouraged: `OKDP_CA_FILE` instead) | `false` | No |
| `SQL_STATEMENT_TIMEOUT` | One SQL editor statement's timeout, PostgreSQL's `statement_timeout` too (chart: `sqlEditor.statementTimeout`) | `2m` | No |
| `SQL_DEFAULT_ROWS` / `SQL_MAX_ROWS` | Row cap of a request that names none / the largest it may ask | `1000` / `10000` | No |
| `SQL_MAX_RESULT_BYTES` | Size of a result's rows past which the rest is dropped | `16777216` | No |
| `SQL_MAX_CONCURRENT_PER_USER` | Statements one user runs at once (`429` past it) | `2` | No |
| `SQL_TOKEN_EXCHANGE_AUDIENCE` | Audience of the token a statement on Trino runs with (chart: `sqlEditor.tokenExchangeAudience`, [SQL editor](#sql-editor)); empty, statements on Trino are refused (`400`, code `sql-no-token-exchange`) | | No |
| `SQL_ALLOWED_NETWORKS` | CIDRs the SQL editor and the connection tests may reach although loopback, link-local or the API server (comma-separated) | | No |
| `OKDP_CA_FILE` | PEM bundle every outbound TLS connection trusts beside the system's CAs ([TLS](#tls); chart: `tls.caBundleSecret` and `tls.caBundleKey`, mounted at `/etc/okdp/ca/<caBundleKey>`). Replaces `SQL_CA_FILE`, refused at startup when still set | | No |
| `SECRET_STORE_ALLOWED_SERVERS` | Vault (OpenBao) server URLs a project's SecretStore and its connection test may name beside the platform's secret store's (comma-separated; chart: `secretStores.allowedServers`) ([secret stores](#secret-stores)) | | No |
| `EXCLUDED_SIDECAR_PREFIXES` | Container name prefixes excluded from pod/metrics views (comma-separated) | `istio-proxy,istio-init,dynatrace-,linkerd-proxy,envoy,vault-agent` | No |

The server provisions and deletes no OAuth client: with
`global.okdp.oidc.clientProvisioning: dcr` the compiled oidc-dcr Jobs register each
instance's clients, with `existing` the platform provisions them, and deleting an instance
never calls Keycloak. `global.okdp.identity.provisioning.provider` is only reported to the
console (`/api/capabilities`); the credentials of `identity.provisioning.keycloak` are not
read.

Users and groups (`/api/v1/identity`, the console's Identity pages) are managed in the
platform Keycloak realm through its Admin REST API. The client named by `KEYCLOAK_CLIENT_ID`
must be confidential, with service accounts enabled, and its service account must hold the
roles `view-users`, `query-users`, `manage-users` and `query-groups` of the realm's
management client: `realm-management` in a regular realm, but `master-realm` in the
`master` realm (there, `realm-management` does not exist; each realm `<r>` has a
`<r>-realm` client in `master`).
Without `KEYCLOAK_CLIENT_SECRET` the identity routes answer `501` and `/api/capabilities`
reports `identity.userManagement: false`. The `comment` and `uid` of a user are Keycloak
user attributes: on Keycloak 24+ the realm user profile must allow them (unmanaged
attributes enabled, or both attributes declared), otherwise Keycloak drops them. An update
reads the user's or group's representation, writes the console's fields over it (a user's
name, email, enabled flag, `comment` and numeric `uid`; a group's `comment`) and puts it
back whole: the other attributes (federated from LDAP, set by an administrator) and fields
are kept.

> For the full chart values (image, service, resources, RBAC), see
> [`chart/values.yaml`](chart/values.yaml).

### TLS

Every outbound TLS connection of the server verifies the certificate it is
shown against the system's CAs and the platform's CA bundle, `OKDP_CA_FILE`: the
chart mounts the key `tls.caBundleKey` (`ca.crt`) of the Secret `tls.caBundleSecret`
of the release namespace at `/etc/okdp/ca/<caBundleKey>` (the platform's
`global.okdp.tls.caBundleSecret`, `certs-bundle` in the sandbox, which trust-manager
writes to every namespace). It covers the OIDC issuer's discovery and keys, the Keycloak
Admin API, the Git repositories (go-git's fetches, pushes and listings, okdp-gitops'
`Credentials.CABundle`), the catalogs, the Forgejo API, Trino, the databases of the SQL
editor, the S3, PostgreSQL and MySQL connection tests (MySQL's `tls: true` verified
with them; `preferred` and `skip-verify` verify nothing) and the secret store test (a
store's `caBundle` beside them). A file that cannot be read or holds no certificate stops the
server at startup. `global.okdp.oidc.insecureSkipVerify` (the OIDC issuer, Keycloak and
Trino) and `KEYCLOAK_TLS_INSECURE` are still honoured, and discouraged: they take any
certificate on trust; give the server the platform's CA instead. The server logs a
`WARNING: insecure setting …` at startup for each one set, naming it, and for
`GITOPS_SSH_INSECURE_IGNORE_HOST_KEY`, `GITOPS_INSECURE_HTTP_HOSTS` and
`INSECURE_OCI_REGISTRIES` too. The upstream charts of
the chart cache are pulled with the system's CAs (okdp-compiler's `chartcache`).

---

## Images

Images are published to [`quay.io/okdp`](https://quay.io/organization/okdp).

| Image | Tag format | Example |
|-------|-----------|---------|
<!-- x-release-please-start-version -->
| `quay.io/okdp/images/okdp-control-plane-server` | `<version>` (matches the chart `appVersion`) | `quay.io/okdp/images/okdp-control-plane-server:0.10.0` |
<!-- x-release-please-end -->

> See the [available tags on quay.io](https://quay.io/repository/okdp/images/okdp-control-plane-server?tab=tags) for all published versions.

---

## Troubleshooting

### The pod restarts in a loop (`CrashLoopBackOff`)

**Cause:** the server builds its Kubernetes client at startup and exits if it cannot.
The logs show `Failed to initialize Kubernetes client: failed to get kubernetes config
(tried in-cluster and local)`. In-cluster this means the ServiceAccount or its RBAC is
missing; for a local run it means `KUBECONFIG` is unset or invalid.

**Fix:** in-cluster, make sure the chart's ServiceAccount and RBAC are applied; locally,
point `KUBECONFIG` at a valid config. Then check the logs and permissions:

```sh
kubectl logs -n okdp-system -l app.kubernetes.io/name=okdp-control-plane-server
kubectl auth can-i get namespaces
```

### API calls fail only in the browser (CORS)

**Cause:** the server returns `Access-Control-Allow-Origin` set verbatim to
`ALLOWED_ORIGINS` (default `http://localhost:4200`). When that value does not match the
origin the console is served from, the browser blocks the response and its console shows
`No 'Access-Control-Allow-Origin' header`. The same requests still work with `curl`.

**Fix:** set `ALLOWED_ORIGINS` to the exact console origin and redeploy.

### The service catalog is empty, or `GET /api/platform-services` returns `500`

**Cause:** the menu is `catalogs.yaml` of the platform repository, the versions
the catalogs of its `repositories`. A `500` means the platform repository cannot be
reached (URL, branch or credentials; the log names the fetch error, a credential refused
over `http://` among them: `GITOPS_INSECURE_HTTP_HOSTS`), or that `catalogs.yaml` holds a
key the compiler does not read (the error names it). An empty but
successful catalog means the file is missing or has no `categories`. Services without
versions mean a catalog cannot be fetched (the log says which), or holds no
`catalog/<service>/` for them.

**Fix:** check the `GITOPS_*` settings (or the topology) and the credentials Secret, that
the file exists on the platform branch, and the `url` and `ref` of each repository.

### A deploy or an update answers `400 … does not compile`

**Cause:** okdp-compiler refused the instance, or another one that compiled before the
write: a parameter its schema refuses, a connection of the wrong contract, a claim no
class binds, a platform value the service requires (`global.okdp…`), an upstream value
the service protects, or a reference to an output no instance publishes. The message
names the instance, the parameter, the chart or the file; `the deployments do not
compile` names a file of the tree that does not load (a package or a version no catalog
has, a file that does not parse).

**Fix:** change what the message names. Nothing was committed. A `500` naming a
catalog (`catalog <name> (<url>@<ref>)`) is the catalog's, not the write's.

### A deployed service stays `Pending`

**Cause:** the instance is committed, but the GitOps engine has not produced its
HelmRelease (`okdp-releases/<project>-<instance>`) or Application (`<project>-<instance>`)
yet. Either the CI of the output has not compiled the commit yet (it runs every few
minutes), or its run failed (a run that fails keeps the output of the project as it
was), or the engine has not reconciled the new revision of the output, or it does not
watch the output (`compiled/`). An instance, the console's or one
declared by hand, stays `Pending` until it is compiled. A HelmRelease whose `dependsOn`
names an instance that is not ready waits for it.

**Fix:** check the last run of the okdp-gitops workflow of the output repository (its log
names what does not compile) and the output's `sources.yaml` (the revisions of the
sources it is the compile of), the engine (`flux get kustomizations`, or the
ApplicationSet in Argo CD), and that `GITOPS_ENGINE` names the engine actually in use.

---

## Contributing & License

Contributions follow the [OKDP contribution guide](https://github.com/OKDP/.github/blob/main/CONTRIBUTING.md). Released under the [Apache License 2.0](LICENSE).

---

**Built 🚀 for the OKDP Community**
<a href="https://okdp.io">
  <img src="https://okdp.io/logos/okdp-notext.svg" height="20px" style="margin: 0 2px;" />
</a>
