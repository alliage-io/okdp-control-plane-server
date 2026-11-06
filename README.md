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

The deployments repositories (topology, go-git writer, layout, the write operations on
the sources, Flux and Argo CD files), the catalogs of packages, the compile of an
instance with okdp-compiler and the chart cache are the packages `gitops`, `catalog`,
`compile` and `charts` of [okdp-gitops](https://github.com/OKDP/okdp-gitops), a module of
its own (`go.mod` replaces it with `../okdp-gitops`, a checkout next to this one, until
it is published).

---

## Architecture

<p align="center">
  <img src="docs/assets/architecture.svg" alt="OKDP Control Plane Server topology" />
</p>

> **OKDP deployment context:** the server runs **in-cluster**. **Git is the only
> desired-state store**: the server never creates HelmReleases, Applications or workloads;
> it commits the sources to the deployments repositories, a CI running `okdp-gitops
> compile` on each of their commits writes the output, and a GitOps engine (FluxCD or
> Argo CD) deploys each instance from its compiled directory in the output repository, the
> same under both engines. What an instance publishes (its URL, its usage, the connections
> it provides) is read from its compiled `okdp.info.yaml`; observed state from the
> cluster: the workloads (`app.kubernetes.io/instance`) and the engine's own objects for
> render/sync errors. There is no database; the local clones are a cache. Authentication
> (OIDC) is verified on every `/api` call; TLS is provided by the platform ingress.

The layout has three parts, each in the repository the topology says (below):

```
platform/catalog.yaml                                   # the catalogs (repositories) and the console menu
platform/projects.yaml                                  # split topology: the projects
projects/<project>/project.yaml                         # {name, description, …}: declares the project
projects/<project>/connections/<name>.yaml              # external connection (credentials stay in a Secret)
projects/<project>/services/<instance>/instance.yaml    # service and version of a package (or a plain chart), connection files
projects/<project>/services/<instance>/values.yaml      # the submitted parameters only
compiled/projects/<project>/kustomization.yaml          # Flux: the compiled services
compiled/projects/<project>/services/<instance>/        # okdp-compiler's umbrella chart (Chart.yaml, instance.values.yaml,
                                                        # okdp.objects.yaml, okdp.info.yaml),
                                                        # helmrelease.yaml, kustomization.yaml (Flux), application.yaml (Argo CD)
```

`projects/` and `platform/` hold what people and the console write (the sources);
`compiled/` (with `flux/` and `argocd/`, the output) is generated from them by
okdp-gitops, in a CI on each commit of the sources, mirrors their paths, and is the only
thing the engines read. The console never writes it: what it reads there (what an
instance publishes, its compiled values) is what the last run wrote, and lags behind the
sources for the time of a run. An instance directory may also hold
the instance's own upstream values, `upstream.<key>.yaml` (written by hand; the server
keeps them).

### Topology: where the parts live

`GITOPS_CONFIG` names a topology file (the chart's `gitops.topology`):

```yaml
platform: {url: <git url>, branch: main, path: platform}
projects: {url: <git url, may contain {project}>, branch: main, path: "projects/{project}"}
output:   {url: <git url>, branch: main, path: ""}   # the directory of compiled/, flux/, argocd/
forgejo:                                             # optional: creates the repository of a new project
  url: http://forgejo-http.forgejo.svc.cluster.local:3000
  owner: okdp
  tokenFile: /etc/okdp/forgejo/token
```

Without it, `GITOPS_REPO_URL`/`GITOPS_BRANCH`/`GITOPS_PATH` hold the whole layout (mono:
platform `<path>/platform`, projects `<path>/projects/{project}`, output `<path>`). Sources
and output may live apart (`io`: one repository of sources, one of output), and each
project in a repository of its own (`split`: `projects.url` names `{project}`; the projects
are listed in `platform/projects.yaml`, `projects: [{name: demo}]`, each entry optionally
overriding `url`, `branch` or `path`). Without `{project}` in its url, `projects.path`
ends with `/{project}`: the projects are the directories of its parent.

A console change in one repository is one commit. Across repositories it is one commit per
repository, with the same message: the projects' repositories, then the platform's (a
repository that moved meanwhile gets the same change on its new revision). The console
writes nothing to the output repository: the CI of the sources compiles each of their
commits into it. In a split topology, creating a project creates
`<owner>/okdp-project-<project>` through the Forgejo API (the token of `tokenFile`), pushes
its `project.yaml` with the workflow `.forgejo/workflows/okdp-gitops.yaml` (its commits
start the compile), then adds it to `platform/projects.yaml`; without a `forgejo` section
that creation fails with a message saying so. Deleting it removes its entry (the next
compile, its compiled tree); its repository is kept.

Projects are the `projects/<project>/project.yaml` files (the namespace is the project
name), whoever wrote them: a project declared by hand in Git is listed, served and edited
by the console like one the console created (in a split topology, the projects of
`platform/projects.yaml`), and `/api/projects/stream` reports the
files as they change (read every few seconds). Creating a project from the console also
creates its Namespace (label `okdp.io/project`) so it can hold Secrets before any
deployment, and refuses a name that is already a Namespace without that label
(`kube-system`, …). Deleting a project removes `projects/<project>/` (the compile of that
commit removes its compiled tree, and the engine uninstalls its releases), then the
Namespace only if it carries the label; a namespace
the engine created is left, as when the directory is removed by hand. An update rewrites
`description` and keeps the other keys and the comments of the file.

Commits read `okdp: <action> <project>/<instance> by <user>`. The logged-in user is the
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
replayed on top of the new revision when someone else pushed in between.

### Compiling an instance

Every write compiles the instances it changes, inside the write, from the sources it
writes, as a check: deploying or updating an instance compiles it; changing a connection
file compiles the instances that layer it in; deleting an instance compiles the project's
other instances, and refuses while one of them references it. A compile that fails
commits nothing, and the API answers `400` with the compiler's message (code
`does-not-compile`). The compiled files are not committed: the CI of the sources compiles
the commit again, the whole layout, and writes them (below,
[the output](#the-output-okdp-gitops)), so a commit made by hand and a console one are
compiled the same way.

An instance names a package: `service` and `version` in `instance.yaml`, the directory
`catalog/<service>/<version>/` of a catalog (below). [okdp-compiler](https://github.com/OKDP/okdp-compiler)
runs it on the instance's inputs, under the release `<project>-<instance>` in the namespace
`<project>`:

| Input | From |
|---|---|
| context, level 0 | `platform/platform-values.yaml`, then every `platform/connections/<name>.yaml` |
| context, level 1 | the `global` and `connections` keys of `projects/<project>/project.yaml`, when set |
| context, level 2 | the project connection files `instance.yaml` names (a platform component has none: its connections are the platform's) |
| values | `values.yaml`, then each `upstream.<key>.yaml` of the instance directory as `upstream: {<key>: ...}` |
| contracts | the union of the catalogs' `contracts/` |
| chart cache | the upstream charts the compile renders, for its checks (below) |

The parameters are checked against the package's `config.schema.json`, the connections
against their contracts, and the values of each upstream chart computed; the instance is
written as one chart, an umbrella over its upstream charts (`compiler.Write`: `Chart.yaml`,
`instance.values.yaml`, OKDP's own objects in `okdp.objects.yaml`, what it publishes in
`okdp.info.yaml`). Each connection reference to another instance of the project (by
its release name, one of the compile's dependencies) becomes a `dependsOn` of the
HelmRelease and a later Argo CD sync wave; it must name an instance of the project, and
an instance another one references cannot be deleted.

Before it commits, the server checks the final values against the upstream charts: it
pulls each chart the umbrella names, from its own repository (an OCI registry or an https
chart repository, anonymously), into its chart cache (`GITOPS_CHART_CACHE_DIR`, laid out
as okdp-compiler reads it, `<repository>/<chart>-<version>/<chart>/`), and compiles again
with it (the chart's `values.schema.json`, the lists an instance appends to). A chart
that cannot be fetched is logged and the instance compiled without the checks: never a
failure. The same cache gives the chart defaults of the rendered values view
(`GET .../services/:serviceName/values`: each chart's defaults coalesced under its
compiled values, the lines set by OKDP or the instance marked).

A **plain chart** (`instance.yaml` with `chart:`, no package: Forgejo, an operator) gets
its values layers merged as Helm merges `-f` files: `platform/platform-values.yaml`, each
connection file of `instance.yaml`, `values.yaml`.

### Packages and catalogs

A package is a directory of a git catalog, in okdp-compiler's format
([writing a package](https://github.com/OKDP/okdp-compiler/blob/main/docs/02_writing_package.md)):

```
catalog/<service>/<version>/   Chart.yaml (name, version, the upstream charts as dependencies),
                               okdp.yaml, values.cel.yaml or values.yaml, config.schema.json (the
                               parameters), <key>.defaults.yaml
contracts/<contract>.schema.json
```

`platform/catalog.yaml` names the catalogs and the console menu:

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

### Generated files: byte-exact format

The output is okdp-gitops', which writes the same bytes whoever runs it (the CI of the
sources, a person), so that two runs never fight; the server reads it back (what an
instance publishes, its compiled values, the labels of the engine objects). Common rules: UTF-8, LF, 2-space
indentation, no tabs, no trailing spaces, no blank lines, a single final newline, first
line `# Generated by okdp compile. Do not edit.`. Values from `instance.yaml` and names are
written in double quotes, without escaping; lists from directories are sorted (`LC_ALL=C`).
The compiled `values.yaml` of a plain chart is that line followed by the values in YAML
(yaml.v3, keys sorted, 2-space indentation); the files of a package's compile are
okdp-compiler's, as `okdp-compile -o` writes them.

`helmrelease.yaml` of a package instance (`{{r}}` the release, `{{p}}` the project,
`{{dir}}` the compiled directory in the output repository; the `dependsOn` block only when
the instance references other instances, sorted): the umbrella of its compiled directory,
built by Flux from the output's `GitRepository` (`okdp-gitops`, in `flux-system`), with
`instance.values.yaml` (through `valuesFrom`: as the chart's own values they would be its
defaults, and lose their nulls):

```yaml
# Generated by okdp compile. Do not edit.
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: "{{r}}"
  namespace: okdp-releases
  labels:
    okdp.io/project: "{{p}}"
    okdp.io/instance: "{{r}}"
spec:
  interval: 10m
  releaseName: "{{r}}"
  targetNamespace: "{{p}}"
  storageNamespace: "{{p}}"
  chart:
    spec:
      chart: "./{{dir}}"
      reconcileStrategy: Revision
      sourceRef:
        kind: GitRepository
        name: okdp-gitops
        namespace: flux-system
  dependsOn:
    - name: "{{dependency}}"
  timeout: 15m
  # install, upgrade, driftDetection: as below
  valuesFrom:
    - kind: ConfigMap
      name: "values-{{p}}-{{i}}"
      valuesKey: instance.values.yaml
```

Of a plain chart of an OCI registry (one of an https chart repository has a
`HelmRepository` and `chart.spec: {chart, version, sourceRef}` instead):

```yaml
# Generated by okdp compile. Do not edit.
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: "{{r}}"
  namespace: okdp-releases
  labels:
    okdp.io/project: "{{p}}"
    okdp.io/instance: "{{r}}"
spec:
  interval: 10m
  url: "{{chart}}"
  ref:
    tag: "{{version}}"
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: "{{r}}"
  namespace: okdp-releases
  labels:
    okdp.io/project: "{{p}}"
    okdp.io/instance: "{{r}}"
spec:
  interval: 10m
  releaseName: "{{r}}"
  targetNamespace: "{{p}}"
  storageNamespace: "{{p}}"
  chartRef:
    kind: OCIRepository
    name: "{{r}}"
  dependsOn:
    - name: "{{dependency}}"
  timeout: 15m
  install:
    createNamespace: true
    strategy:
      name: RetryOnFailure
      retryInterval: 2m
  upgrade:
    remediation:
      retries: 3
      strategy: rollback
  driftDetection:
    mode: enabled
  valuesFrom:
    - kind: ConfigMap
      name: "values-{{p}}-{{i}}"
      valuesKey: values.yaml
```

`kustomization.yaml` of an instance (`instance.values.yaml` for a package), and of a
project (`resources: []` without services):

```yaml
# Generated by okdp compile. Do not edit.
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: okdp-releases
resources:
  - helmrelease.yaml
configMapGenerator:
  - name: "values-{{p}}-{{i}}"
    files:
      - values.yaml
    options:
      disableNameSuffixHash: true
      labels:
        reconcile.fluxcd.io/watch: Enabled
```

```yaml
# Generated by okdp compile. Do not edit.
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: okdp-releases
resources:
  - services/{{i}}
```

A compile of the whole repository also writes:

- `compiled/platform/components/<NN>-<name>/`: the same three files for each platform
  component (`platform/components/<NN>-<name>/`: `instance.yaml`, `values.yaml`, its
  connection files under `platform/connections/`);
- `compiled/platform/kustomization.yaml`, the ConfigMap `okdp-platform-values` the
  server reads, from `compiled/platform/platform-values.yaml`, a copy of
  `platform/platform-values.yaml` (kustomize reads no file outside its directory);
- `flux/components.yaml`: one Flux `Kustomization` per component, path
  `./<prefix>/compiled/platform/components/<NN>-<name>`, `wait: true`, depending on every
  component of the nearest lower non-empty layer (`00`, `10`, `20`, `30`; the lowest on
  `okdp-platform-values`), then `okdp-projects` (path `./<prefix>/compiled/projects`, no
  `wait`) depending on the highest layer.

`application.yaml`, when the output holds `argocd/root.yaml` (an Argo CD Application with
`spec.source.repoURL`, the repository the engine reads the output from): the Application
`argocd/<p>-<i>` of the instance, reading the same repository and `targetRevision`: a
package's compiled directory as a Helm source with `valueFiles: [instance.values.yaml]`,
a plain chart's chart with the compiled `values.yaml` as `$values`. Its
`argocd.argoproj.io/sync-wave` is 0 without dependency, one more than the instances it
depends on otherwise, plus `<NN>` for a platform component, for an app-of-apps to order
them. Without `argocd/root.yaml`, no Application is written.

The golden files of okdp-gitops' `gitops/testdata/golden` are the reference of the plain
charts, its `compile/testdata` the fixtures of the packages. The first compile of a
repository whose Flux files were generated next to the sources (`# Generated by
scripts/render-flux.sh`) removes them; a compiled directory whose sources were removed
goes with the next compile.

### The output: `okdp-gitops`

[okdp-gitops](https://github.com/OKDP/okdp-gitops) writes the output: a CI runs
`okdp-gitops compile` on each commit of the sources, the console's and those made by
hand alike, and commits what changed to the output repository (`okdp: compile`), which
the engines reconcile. It is no longer built from this repository; from a checkout of
okdp-gitops (next to one of okdp-compiler):

```sh
make build
bin/okdp-gitops compile --root <deployments repository>/gitops          # write what changed
bin/okdp-gitops compile --root <deployments repository>/gitops --check  # fail when stale
bin/okdp-gitops compile --config topology.yaml --catalog platform-charts=../platform-charts
bin/okdp-gitops workflow --branch main                                  # the CI workflow of a repository of the sources
```

`--root DIR` is a layout whose parts are all in one local directory; `--config FILE` a
topology file, whose urls are git repositories (cloned; the changes committed and pushed
as `okdp: compile`, with the credentials of `$GITOPS_CREDENTIALS_DIR`) or local
directories (an absolute path, or one relative to the file: read and written in place).
`--catalog NAME=DIR` reads the catalog `NAME` of `platform/catalog.yaml` from a local
checkout instead of its url; `--charts DIR` checks the values against the upstream
charts, pulled into that chart cache, as the server checks them.

It checks every declaration first (the names, the keys of `instance.yaml` and their
types, the connection files, the release names unique across projects and components,
the layers), compiles every project service and platform component, and rewrites the
generated files above; it removes the ones no source produces any more, and the Flux
files `render-flux.sh` wrote next to the sources. Every problem is reported at once, and
nothing is written: the engines keep deploying the last output. `--path-prefix` is the
path of the output in its Git repository, for the paths of the engine files (default:
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
  from the output repository of the deployments layout above
- A CI running [okdp-gitops](https://github.com/OKDP/okdp-gitops) on each commit of the
  sources (Forgejo Actions, `.forgejo/workflows/okdp-gitops.yaml`): it writes the output
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

The platform values (`global.okdp`) are read from the ConfigMap `okdp-platform-values`
(key `values.yaml`) that Flux generates in `okdp-releases`, or from
`platform/platform-values.yaml` in Git when there is none (Argo CD). A compile (the
server's check, the CI's) always reads `platform/platform-values.yaml` from the sources it
compiles.

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

With the parts of the layout in several repositories, set `gitops.topology` instead of
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
| `GITOPS_REPO_URL` | Deployments repository holding the whole layout (`https://…`, `ssh://…`, `git@host:path`, `file://…`) | | One of the two |
| `GITOPS_BRANCH` | Branch holding the desired state | `main` | No |
| `GITOPS_PATH` | Directory of the repository holding the layout | repository root | No |
| `GITOPS_CREDENTIALS_DIR` | Mounted credentials Secret (`username`/`password`, `bearerToken`, or `identity`/`known_hosts`), for every deployments repository, and the catalogs on their host | `/etc/okdp/gitops-credentials` | No |
| `GITOPS_SSH_INSECURE_IGNORE_HOST_KEY` | Accept any SSH host key (sandboxes only) | `false` | No |
| `GITOPS_AUTHOR_NAME` / `GITOPS_AUTHOR_EMAIL` | Commit committer (and author when the user has no email; the logged-in user authors the commit otherwise) | `OKDP control plane` / `okdp-control-plane@okdp.io` | No |
| `GITOPS_CLONE_DIR` | Local clones, a cache, one per repository under `repos/` (empty: in memory) | in memory | No |
| `GITOPS_CHART_CACHE_DIR` | Chart cache: the upstream charts of the compiled instances | `<GITOPS_CLONE_DIR>/charts`, else under the temporary directory | No |
| `GITOPS_ENGINE` | `flux` or `argocd`: whose objects carry render/sync errors | `flux` | No |
| `GITOPS_RELEASES_NAMESPACE` | Flux HelmReleases and values ConfigMaps, `okdp-platform-values` included | `okdp-releases` | No |
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

**Cause:** the menu is `platform/catalog.yaml` of the platform repository, the versions
the catalogs of its `repositories`. A `500` means the platform repository cannot be
reached (URL, branch or credentials; the log names the fetch error). An empty but
successful catalog means the file is missing or has no `categories`. Services without
versions mean a catalog cannot be fetched (the log says which), or holds no
`catalog/<service>/` for them.

**Fix:** check the `GITOPS_*` settings (or the topology) and the credentials Secret, that
the file exists on the platform branch, and the `url` and `ref` of each repository.

### A deploy or an update answers `400 … does not compile`

**Cause:** okdp-compiler refused the instance: a parameter its schema refuses, a
connection of the wrong contract, a platform value the service requires
(`global.okdp…`), an upstream value the service protects, or a reference to an instance
the project does not have. The message names the parameter, the chart or the file.

**Fix:** change what the message names. Nothing was committed. A `500` naming a
catalog (`catalog <name> (<url>@<ref>)`) or a `package` is the catalog's, not the
instance's; a package or a version no catalog has is the instance's (`400`).

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
