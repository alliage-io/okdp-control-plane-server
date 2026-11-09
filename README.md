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
[okdp-gitops](https://github.com/OKDP/okdp-gitops) writes it, in a CI on each commit of
the sources.

It is a first-party OKDP component, developed alongside the web console. Cluster credentials
and business logic stay on the server side, keeping the console a stateless browser client.

---

## What the project does

- **Docker image** `quay.io/okdp/images/okdp-control-plane-server`: runs the Go control-plane API
  server on a minimal Alpine base, listening on port `8093`.
- **Helm chart** (`chart/`): deploys the server in-cluster with its Git settings and the
  read-mostly RBAC it needs on the Kubernetes API.

The image and chart provide the backend for the OKDP web console, so the platform is
driven through the console instead of hand-written GitOps files. Both paths produce the
same files: a console deployment and a hand-written one are indistinguishable.

Project layout:

- `cmd/server`: entry point.
- `internal/api`: HTTP handlers and router.
- `internal/service`: business logic.
- `internal/repository`: cluster and repository access (platform values, what the instances publish, GitOps engine status).
- `internal/config`: configuration loaded from environment variables.
- `chart/`: Helm chart.

The deployments repositories (topology, go-git writer, the transactions on the tree, each
checked by a compile of the whole tree), the compile of the tree with okdp-compiler (each
instance's outcome, the claims bound to each provider), the catalogs of packages and the
chart cache are the packages `gitops`, `catalog` and `charts` of
[okdp-gitops](https://github.com/OKDP/okdp-gitops), a module of its own (`go.mod` replaces
it with `../okdp-gitops`, a checkout next to this one, until it is published).

---

## Architecture

<p align="center">
  <img src="docs/assets/architecture.svg" alt="OKDP Control Plane Server topology" />
</p>

> **OKDP deployment context:** the server runs **in-cluster**. **Git is the only
> desired-state store**: the server never creates HelmReleases, Applications or workloads;
> it commits the tree to the deployments repositories, a CI running `okdp-gitops
> compile` on each of their commits writes the output, and a GitOps engine (FluxCD or
> Argo CD) deploys each instance from its compiled directory in the output repository, the
> same under both engines. What an instance publishes (its URL, its usage, the connections
> it provides) is read from its compiled `okdp.info.yaml`; observed state from the
> cluster: the workloads (`app.kubernetes.io/instance`) and the engine's own objects for
> render/sync errors. There is no database; the local clones are a cache. Authentication
> (OIDC) is verified on every `/api` call; TLS is provided by the platform ingress.

The tree is okdp-compiler's
([repository layout](https://github.com/OKDP/okdp-compiler/blob/main/docs/01_usage.md#repository-layout)),
its parts each in the repository the topology says (below):

```
platform.yaml                                           # the platform: global.okdp, the classes of the claims, the platform's connections
catalogs.yaml                                           # okdp-gitops': the catalogs (repositories) and the console menu
projects.yaml                                           # okdp-gitops', split topology: the projects
projects/<project>/project.yaml                         # declares the project: its globals, classes, connections, named claims
projects/<project>/instances/<instance>/instance.yaml   # {service, version}: the package
projects/<project>/instances/<instance>/config.yaml     # the submitted parameters only (none without parameters)
compiled/projects/<project>/instances/<project>-<instance>/
                                                        # okdp-compiler's umbrella chart (Chart.yaml, instance.values.yaml,
                                                        # okdp.objects.yaml, okdp.info.yaml), helmrelease.yaml,
                                                        # kustomization.yaml (Flux), application.yaml (Argo CD)
```

`platform.yaml`, `catalogs.yaml` and `projects/` hold what people and the console write
(the tree); `compiled/` (the output) is generated from them by okdp-gitops, the files of
okdp-compiler's Flux and Argo CD targets, in a CI on each commit of the tree, and is the
only thing the engines read. The console never writes it: what it reads there (what an
instance publishes, its compiled values) is what the last run wrote, and lags behind the
tree for the time of a run. The platform's own services (the database servers, the
ingress, the control plane) are instances of projects too: there are no platform
components, and the deployment graph (connection references, claims) is the only order.

### Topology: where the parts live

`GITOPS_CONFIG` names a topology file (the chart's `gitops.topology`):

```yaml
platform: {url: <git url>, branch: main, path: ""}   # the directory of platform.yaml, catalogs.yaml
projects: {url: <git url, may contain {project}>, branch: main, path: "projects/{project}"}
output:   {url: <git url>, branch: main, path: ""}   # the directory of compiled/ (and argocd/root.yaml)
forgejo:                                             # optional: creates the repository of a new project
  url: http://forgejo-http.forgejo.svc.cluster.local:3000
  owner: okdp
  tokenFile: /etc/okdp/forgejo/token
```

Without it, `GITOPS_REPO_URL`/`GITOPS_BRANCH`/`GITOPS_PATH` hold the whole tree (mono:
platform `<path>`, projects `<path>/projects/{project}`, output `<path>`). The tree and the
output may live apart (`io`: one repository of the tree, one of output), and each project
in a repository of its own (`split`: `projects.url` names `{project}`; the projects are
listed in `projects.yaml`, beside `platform.yaml`, `projects: [{name: demo}]`, each entry
optionally overriding `url`, `branch` or `path`). Without `{project}` in its url,
`projects.path` ends with `/{project}`: the projects are the directories of its parent.

A console change in one repository is one commit. Across repositories it is one commit per
repository, with the same message: the projects' repositories, then the platform's (a
repository that moved meanwhile gets the same change on its new revision). The console
writes nothing to the output repository: the CI of the sources compiles each of their
commits into it. In a split topology, creating a project creates
`<owner>/okdp-project-<project>` through the Forgejo API (the token of `tokenFile`), pushes
its `project.yaml` with the workflow `.forgejo/workflows/okdp-gitops.yaml` (its commits
start the compile), then adds it to `projects.yaml`; without a `forgejo` section that
creation fails with a message saying so. Deleting it removes its entry (the next compile,
its compiled tree); its repository is kept.

Projects are the `projects/<project>/project.yaml` files (the namespace is the project
name), whoever wrote them: a project declared by hand in Git is listed, served and edited
by the console like one the console created (in a split topology, the projects of
`projects.yaml`), and `/api/projects/stream` reports the files as they change (read every
few seconds). A project's description is the key `description` of its `project.yaml`,
the project's own (okdp-compiler leaves it out of the context of the project's instances);
an update rewrites that key and keeps the rest of the file. Creating a project from the console also creates its Namespace (label
`okdp.io/project`) so it can hold Secrets before any deployment, and refuses a name that
is already a Namespace without that label (`kube-system`, …). Deleting a project removes
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
passes `/usr/bin/git describe`; `dev` otherwise), with the committer email. The author comes from
the access token: the `email` claim (Keycloak: the `email` client scope on the console
client) and the `name` claim (the `profile` scope; the user name when absent). Without an
email (the claim is missing or not an address, or authentication is disabled) the GitOps
identity authors the commit too, the user is named in the subject only, the message has
no trailer, and the server logs a warning once per user. CR, LF, `<` and `>` are stripped
from the name and email. Writes are serialised and
replayed on top of the new revision when someone else pushed in between.

### Checking a write

Every write is checked, inside the write, by a compile of the whole tree it leaves
(okdp-gitops: `compiler.LoadRepository` of the tree and the catalogs, `CompileAll`,
`NewGraph`), compared with the compile of the tree before it: deploying or updating an
instance, changing a connection, a named claim or a project. It is refused, nothing
committed, when the instance it writes does not compile, when an instance that compiled
before does not any more (a class, a named claim or a connection of a project reaches
every instance of it, a claim binds across projects), or when the tree no longer loads;
the API answers `400` with the compiler's message (code `does-not-compile`). Removing an
instance, a connection, a named claim or a project is refused (`400`, naming them) while
instances use it: they would not compile without it. The compiled files are not committed:
the CI of the sources compiles the commit again, the whole tree, and writes them (below,
[the output](#the-output-okdp-gitops)), so a commit made by hand and a console one are
compiled the same way.

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
upstream charts (`Chart.yaml`, `instance.values.yaml`, OKDP's own objects in
`okdp.objects.yaml`, what it publishes in `okdp.info.yaml`). Each connection reference to
another instance (an output it publishes, by its name, or a claim it provisions) becomes a
`dependsOn` of the HelmRelease and a later Argo CD sync wave.

The server compiles with a chart cache: it pulls each chart the umbrellas name, from its
own repository (an OCI registry or an https chart repository, anonymously), into its chart
cache (`GITOPS_CHART_CACHE_DIR`, laid out as okdp-compiler reads it,
`<repository>/<chart>-<version>/<chart>/`), and compiles again with it (the chart's
`values.schema.json`, the lists an instance appends to). A chart that cannot be fetched is
logged and the tree compiled without the checks: never a failure. The compile of the same
tree and catalog revisions is kept, for the reads that follow. The same cache gives the
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
| `POST /api/projects/:name/claims`, `PUT`, `DELETE .../claims/:claimName` | declare, change, remove a named claim in `project.yaml` (its comments kept): checked (the contract, the class or the provider, the request against `x-okdp-claim` and the class), then by the compile of the tree, refused when an instance that compiled stops compiling; a named claim an instance names is not removed |
| `GET /api/projects/:name/provisioning` | the classes of each contract's claims, the platform's and the project's, the default class, the claim schema |
| `GET /api/projects/:name/services/:serviceName/claims` | the claims of the instance's references, each bound and what its provider granted (`okdp.info.yaml`); for a provider (its package's `provisions`), the claims bound to it, their claimers and what it granted each |

Deploying or updating an instance checks the claims its parameters write or name before
the compile does: the keys of a claim (`class`, `provider`, `request`), its class (of the
project or the platform), its request (the package's fixed one, the instance's merged
over it) against the contract's claim schema and what the class fixes. The deployment
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

`catalogs.yaml`, beside `platform.yaml`, names the catalogs and the console menu:

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

[okdp-gitops](https://github.com/OKDP/okdp-gitops) writes the output: a CI runs
`okdp-gitops compile` on each commit of the tree, the console's and those made by hand
alike, and commits what changed to the output repository (`okdp: compile`), which the
engines reconcile. `compiled/` holds the files of okdp-compiler's targets as they write
them ([targets](https://github.com/OKDP/okdp-compiler/blob/main/docs/specs/01_inputs_outputs.md#targets)):
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
bin/okdp-gitops compile --root <deployments repository>/gitops          # write what changed
bin/okdp-gitops compile --root <deployments repository>/gitops --check  # fail when stale
bin/okdp-gitops compile --config topology.yaml --catalog platform-charts=../platform-charts
bin/okdp-gitops workflow --branch main                                  # the CI workflow of a repository of the sources
```

`--root DIR` is a tree whose parts are all in one local directory; `--config FILE` a
topology file, whose urls are git repositories (cloned; the changes committed and pushed
as `okdp: compile`, with the credentials of `$GITOPS_CREDENTIALS_DIR`) or local
directories (an absolute path, or one relative to the file: read and written in place).
`--catalog NAME=DIR` reads the catalog `NAME` of `catalogs.yaml` from a local checkout
instead of its url; `--charts DIR` checks the values against the upstream charts, pulled
into that chart cache, as the server checks them. Every problem of the tree is reported at
once, and nothing is written: the engines keep deploying the last output. `--path-prefix`
is the path of the output in its Git repository, for the paths the engines read (default:
the topology's output path; with `--root`, from `git`, `""` at the repository root).

Each repository of the sources holds the workflow `.forgejo/workflows/okdp-gitops.yaml`
(Forgejo Actions, `okdp-gitops workflow`), run on a runner dedicated to okdp-gitops: the
server writes it in the repository it creates for a project of a split topology, the
others get it by hand. The runner it needs is described in
[okdp-gitops' README](https://github.com/OKDP/okdp-gitops#the-ci-of-the-sources).

---

## Requirements

- Kubernetes cluster reconciled by [FluxCD](https://fluxcd.io) (helm-controller,
  source-controller, kustomize-controller) or [Argo CD](https://argo-cd.readthedocs.io),
  from the output repository of the deployments above
- A CI running [okdp-gitops](https://github.com/OKDP/okdp-gitops) on each commit of the
  tree (Forgejo Actions, `.forgejo/workflows/okdp-gitops.yaml`): it writes the output
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

The server runs in-cluster, next to a GitOps engine reconciling the deployments
repository. Give it write access through a Secret using the Flux `GitRepository` keys:

```sh
kubectl create secret generic okdp-gitops-credentials -n okdp-system \
  --from-literal=username=okdp-console --from-literal=password=<token>
# or, over SSH: --from-file=identity=./id_ed25519 --from-file=known_hosts=./known_hosts
```

The platform values (`global.okdp`) are read from `platform.yaml` in Git, the context
every instance is compiled with (the server's check, the CI's).

Install the chart from the OKDP registry:

<!-- x-release-please-start-version -->
```sh
helm install okdp-control-plane-server oci://quay.io/okdp/charts/okdp-control-plane-server --version 0.10.0 \
  -n okdp-system --create-namespace \
  --set gitops.repoURL=https://git.example.com/okdp/deployments.git \
  --set gitops.credentialsSecret=okdp-gitops-credentials \
  --set gitops.engine=flux
```
<!-- x-release-please-end -->

With the parts of the tree in several repositories, set `gitops.topology` instead of
`gitops.repoURL` (written to a ConfigMap mounted at `/etc/okdp/gitops/topology.yaml`), and
`gitops.forgejoTokenSecret` (a Secret with the key `token`) for the server to create the
repository of a new project.

> This chart name is published on Quay once the rename lands upstream. Until
> then, install from `chart/` in this checkout.

Once the pod is `Running`, reach the API through a port-forward:

```sh
kubectl port-forward -n okdp-system svc/okdp-control-plane-server 8093:8093
curl -s http://localhost:8093/health
# {"status":"ok"}
```

The Swagger UI is then available at `http://localhost:8093/swagger/index.html`.

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
connects to the Kubernetes API at startup) and a deployments repository: any Git URL
works, a local bare repository included (`git init --bare /tmp/deployments.git`).

**On your machine.** Install Go and the development tools (`kubectl`, `air`,
`swag`, `golangci-lint`, `delve`), then:

```sh
export KUBECONFIG=<path-to-your-kubeconfig>
export GITOPS_REPO_URL=file:///tmp/deployments.git
make dev                 # hot-reload on :8093
# or, without hot-reload:
go run ./cmd/server
```

**In the devcontainer.** Only Docker is required: open the repository and select "Reopen
in Container" (or run `devcontainer up`). The container ships the full toolchain,
publishes port 8093 and syncs a container-reachable copy of the host kubeconfig at
startup. Then run `make dev`.

### Build, test and lint

No cluster is required, but okdp-compiler and okdp-gitops checked out next to this
repository (`go.mod` replaces them with `../okdp-compiler` and `../okdp-gitops` until
they are published):

```sh
make build     # compile the binary to bin/server
make test      # run the tests
make lint      # run golangci-lint
make swagger   # regenerate the Swagger documentation
docker build --build-context okdp-compiler=../okdp-compiler \
  --build-context okdp-gitops=../okdp-gitops -t okdp-control-plane-server .
```

Run `make help` for the full list of targets.

---

## Configuration

The server is configured through environment variables, rendered by the chart from
its `configuration:` values (see [`chart/values.yaml`](chart/values.yaml)).

| Parameter | Description | Default | Required |
|-----------|-------------|---------|:--------:|
| `PORT` | HTTP port the server listens on | `8093` | No |
| `PLATFORM_NAMESPACE` | Namespace where the OKDP platform runs | `okdp-system` | No |
| `ALLOWED_ORIGINS` | Single CORS origin, set verbatim in `Access-Control-Allow-Origin` (the console URL) | `http://localhost:4200` | No |
| `LOG_LEVEL` | Log verbosity (`debug`, `info`, `warn`, `error`) | `info` | No |
| `GIN_MODE` | HTTP framework mode (`debug` prints every route) | `release` | No |
| `GITOPS_CONFIG` | Topology file: the repositories of the platform, the projects and the output, and Forgejo ([topology](#topology-where-the-parts-live)) | | One of the two |
| `GITOPS_REPO_URL` | Deployments repository holding the whole tree (`https://…`, `ssh://…`, `git@host:path`, `file://…`) | | One of the two |
| `GITOPS_BRANCH` | Branch holding the desired state | `main` | No |
| `GITOPS_PATH` | Directory of the repository holding the tree | repository root | No |
| `GITOPS_CREDENTIALS_DIR` | Mounted credentials Secret (`username`/`password`, `bearerToken`, or `identity`/`known_hosts`), for every deployments repository, and the catalogs on their host | `/etc/okdp/gitops-credentials` | No |
| `GITOPS_SSH_INSECURE_IGNORE_HOST_KEY` | Accept any SSH host key (sandboxes only) | `false` | No |
| `GITOPS_AUTHOR_NAME` / `GITOPS_AUTHOR_EMAIL` | Commit committer (and author when the user has no email; the logged-in user authors the commit otherwise) | `OKDP control plane` / `okdp-control-plane@okdp.io` | No |
| `GITOPS_CLONE_DIR` | Local clones, a cache, one per repository under `repos/` (empty: in memory) | in memory | No |
| `GITOPS_CHART_CACHE_DIR` | Chart cache: the upstream charts of the compiled instances | `<GITOPS_CLONE_DIR>/charts`, else under the temporary directory | No |
| `GITOPS_ENGINE` | `flux` or `argocd`: whose objects carry render/sync errors | `flux` | No |
| `GITOPS_RELEASES_NAMESPACE` | Flux HelmReleases and their values ConfigMaps | `okdp-releases` | No |
| `ARGOCD_NAMESPACE` | Argo CD Applications | `argocd` | No |
| `INSECURE_OCI_REGISTRIES` | Registry hosts reached over plain HTTP to pull the upstream charts into the chart cache (sandboxes) | | No |
| `KEYCLOAK_CLIENT_ID` | Keycloak service-account client of the platform realm (chart: `keycloak.credentialsSecret`, key `client_id`) | `okdp-control-plane` | No |
| `KEYCLOAK_CLIENT_SECRET` | Its secret (chart: `keycloak.credentialsSecret`, key `client_secret`). Unset: user and group management is off | | No |
| `KEYCLOAK_URL` / `KEYCLOAK_REALM` | Keycloak base URL and realm of the Admin API | from the platform OIDC issuer `<url>/realms/<realm>` | No |
| `KEYCLOAK_TLS_INSECURE` | Skip the Keycloak certificate check (sandboxes only) | `false` | No |
| `EXCLUDED_SIDECAR_PREFIXES` | Container name prefixes excluded from pod/metrics views (comma-separated) | `istio-proxy,istio-init,dynatrace-,linkerd-proxy,envoy,vault-agent` | No |

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
attributes enabled, or both attributes declared), otherwise Keycloak drops them.

> For the full chart values (image, service, resources, RBAC), see
> [`chart/values.yaml`](chart/values.yaml).

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
reached (URL, branch or credentials; the log names the fetch error). An empty but
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
yet. Either the CI of the sources has not compiled the commit yet, or its run failed (a
run that fails writes nothing), or the engine has not reconciled the new revision of the
output, or it does not watch the output (`compiled/`). An instance, the console's or one
declared by hand, stays `Pending` until it is compiled. A HelmRelease whose `dependsOn`
names an instance that is not ready waits for it.

**Fix:** check the run of the okdp-gitops workflow on the commit (its log names what does
not compile), the engine (`flux get kustomizations`, or the ApplicationSet in Argo CD),
and that `GITOPS_ENGINE` names the engine actually in use.

---

## Contributing & License

Contributions follow the [OKDP contribution guide](https://github.com/OKDP/.github/blob/main/CONTRIBUTING.md). Released under the [Apache License 2.0](LICENSE).

---

**Built 🚀 for the OKDP Community**
<a href="https://okdp.io">
  <img src="https://okdp.io/logos/okdp-notext.svg" height="20px" style="margin: 0 2px;" />
</a>
