# github-action-workflow

GitHub Actions workflow repository.

## Installing shared application secrets

Secret management lives in the same `github-terraform-resource/infra/main.tf`
root as repository creation, teams and OAuth metadata. Its secret module uses
`depends_on = [module.custom]` to wait for repository creation. There is no
separate replication workflow, Terraform root or secret state.

Source values are **repository Actions secrets** in
`Crafted-Beyond/github-terraform-resource` once:

| Secret | Value |
|---|---|
| `GH_TOKEN` | Token authorized to manage destination repository secrets and access the private state backend |
| `DOCKERHUB_TOKEN` | Shared Docker Hub registry token |
| `KUBECONFIG` | Complete self-contained kubeconfig with runner-reachable server and embedded credentials |

`DOCKERHUB_USERNAME = "john24rel"` and
`CHART_REPOSITORY_URL = "https://github.com/Crafted-Beyond/crb-craftedbeyond-k3s-chart.git"`
are plain values defined in the infrastructure module call. Terraform creates
them as repository Actions **variables** for `secret = true` destinations;
callers use `vars.DOCKERHUB_USERNAME` and `vars.CHART_REPOSITORY_URL`.
They do not need source secrets. Existing manually created secrets with these
names are not deleted, but updated callers no longer read them.

Replace any client credential shared in chat before uploading it. Never commit
secret values. `GH_TOKEN` also needs the application workflow permissions:
application tag/PR write, collaborator-permission read, infrastructure/chart read,
backend repository read/write, and Secrets write for every destination.
Complete any organization SSO approval. Keep an old token valid until its
replacement has propagated.

In `github-terraform-resource/infra/main.tf`, add `secret = true` inside each
desired `sub_repos` entry. It defaults to false; `wireguard-api` is enabled.
Do not enable it on the source `github-terraform-resource` repository.
With `secret = true` and no explicit list, only `GH_TOKEN` is copied.
Use `dedicated_secrets = ["GH_TOKEN", "DOCKERHUB_TOKEN", "KUBECONFIG", "APPLICATION_SECRETS"]`
to copy exactly those names. `secret = false` disables copying.
The infrastructure caller explicitly passes source values as a JSON
`TF_VAR_repository_secret_values` assignment through the existing `tofu_env`
secret. No new reusable workflow secret input is needed.
Add further names to that mapping using `toJSON(secrets.YOUR_NAME)`;
GitHub does not expose stored secret plaintext through its API.

Run **Manage GitHub infrastructure** in `github-terraform-resource` on `main`
with action **apply**. That same plan/apply creates opted-in secrets and updates
changed source values. Plans and pull requests never write secrets.
There is no schedule and changing a secret alone does not start an apply.
Local CLI use requires securely providing `TF_VAR_github_token` and
`TF_VAR_repository_secret_values` to the same infrastructure root/state.

Publish both repositories' configuration changes to `main` before using this
integration. The existing apply uses the `prod` GitHub environment, so approval
may be required. Review environment overrides of repository secrets.

**State contains plaintext secret values**, even though Terraform marks them
sensitive in CLI output. The existing unified state is saved in the private
`Crafted-Beyond/github-terraform-backend` repository at
`github-terraform-resource/prod/terraform.tfstate`. Restrict backend access: Git history
retains previous values. Plans/state are not uploaded as workflow artifacts.
Partial applies are saved; if state saving fails, stop and reconcile state
before rerunning because the hosted runner is ephemeral.

Only the shared and explicitly selected dedicated secrets are managed;
unrelated secrets remain untouched.
Destination values with those names are overwritten. Secret resources have
`prevent_destroy`: opting out, renaming or removing a destination that already
received secrets will fail planning until ownership/removal is explicitly
reviewed. This avoids automatically deleting deployed credentials.
Only opt in trusted repositories; the same token and cluster identity are copied.
Destination environment secrets with matching names override repository secrets.
Successful replication does not prove deployment connectivity or trigger builds.

**Application-specific secrets require explicit selection.** For example,
`wireguard-api` requires `APPLICATION_SECRETS` containing PostgreSQL and admin
passwords. Populate that source value and opt it in with `dedicated_secrets`,
or manage it separately; shared secrets alone are insufficient.
Registry repositories, runner connectivity and published trusted workflows,
Terraform modules and charts must also be configured.

## Docker image builds and semantic releases

Caller run titles identify PR image requests, combine-deploy previews and merged
release image builds, with the event's labels and PR number. Manual combine
runs show the destination environment and requested tag (or latest stable).
Publish the updated caller as well as the reusable workflows to see these names.
Titles describe the request/event, not authorization or success: controller
validation still rejects missing/conflicting labels and unauthorized commands.
Job/step names distinguish build from deployment and display resolved image tags.
Run titles retain the labels captured when the event was created; later label
edits do not rename an existing run.

Reusable workflow: [docker-image.yml](./.github/workflows/docker-image.yml).
Copy the [caller example](./examples/docker-image-caller.yml) into an application's
`.github/workflows/` directory and replace its image name. Docker Hub is the
default. Build preflight creates a missing Docker Hub repository as **private**
using the configured namespace/name, then verifies repository and tag-list
access. Existing repositories retain their visibility. The Docker Hub credential
must have repository-creation permission in that namespace, and the account
must have available private-repository capacity; otherwise preflight fails
explicitly. Custom registry adapters retain their existing provisioning behavior.

The caller must include comment events and merged-PR events:

```yaml
on:
  pull_request:
    branches: [main]
    types: [closed]
  issue_comment:
    types: [created]
```

Do not use `pull_request_target`. Forks are skipped. Same-repository PR authors
and anyone allowed to issue build commands must be trusted: the Dockerfile
builds before merge with access to a registry-authenticated builder. Use a
dedicated narrowly scoped registry token, protect the release branch, and
restrict workflow/label changes. This is not an untrusted public-PR build service.

### Labels and tags

| PR event/state | Result |
| --- | --- |
| Label added, PR opened/reopened, or new commit pushed | No build |
| New commit arrives after a command is accepted | Finish that command's captured SHA; a new command is required for the newer commit |
| Open/reopened PR: `major` label + new `/major` comment | Build current PR **head** commit |
| Open/reopened PR: `minor` label + new `/minor` comment | Build current PR head commit |
| Open/reopened PR: `patch` label + new `/patch` comment | Build current PR head commit |
| Command does not match the single release label | Reject and explain the required command |
| `combine-deploy` without a release label | Reject, even with `/combine-deploy` |
| `combine-deploy` + one release label + `/combine-deploy` | Build and preview-deploy through `combine-workflow` only |
| Two or more release labels | Fail before building |
| Closed without merge | No release |
| Merged into `main` with exactly one release label | Build exact merged commit and publish semantic version |

Commands must be standalone `/major`, `/minor`, `/patch` or `/combine-deploy`
comments. The controller captures the current PR SHA when it accepts the command
in `prepare`, before waiting for the publication lock. Build checkout, image tag,
finalization and preview deployment retain that SHA even if the PR head advances.
This is the SHA observed at command acceptance, not necessarily at comment creation
if the runner was queued. Closing the PR, invalidating labels or changing its
repository authorization still blocks the run.
For Docker Hub PR images, preparation checks the exact `pr<number>-<last8SHA>`
tag after validating its full-SHA reservation. If present with a valid manifest
digest, it skips checkout/build/push and reuses that digest. Lookup/authentication
errors fail explicitly rather than triggering an overwrite. Missing tags build
normally; custom adapters retain their existing build behavior. Image finalization,
authorization checks and `/combine-deploy` deployment still run. Terraform plans
the desired infrastructure even when the image is reused; it may report no
changes. Reusing a commit does not rebuild for changes outside that commit, such
as an updated base image or different build settings.
Only new comment events trigger PR work; historical comments are not
searched or reused on later commits. A commenter needs write, maintain or admin
permission. The PR must currently be open, including after reopening. Rerunning
a failed run is recovery, not a new push trigger. Use only one caller (Docker-only
or combined) per application, not both.

Temporary image tags are `pr<number>-<last8SHA>`,
for example `pr1-d56b7b41`.
The PR prefix is deliberate: bare SHA tags can be shared by different PRs,
making safe PR-specific deletion impossible. These tags never contain a
`major`/`minor`/`patch` version. The full SHA is used for checkout and OCI revision
metadata. Preparation checks the selected SHA against the PR commit history and
rejects suffix collisions within that history. No Git tags are created for PRs.
This check does not prove ownership of manually overwritten registry tags or
commits removed by force-push; do not manually overwrite workflow image tags.
Changing one release label to another for the same SHA reuses that PR/SHA tag
only when a new matching command is posted.
Removing the last label does not remove existing images or start a new build.

After a successful PR publication and verification that its new tag exists,
delete older temporary tags **for that PR only**, keeping the current one.
Old images are not deleted before their replacement is published. Cleanup
also retries tags left by earlier failed cleanup, rather than only deleting
one remembered previous tag.
This also applies to `/combine-deploy`: cleanup runs in image finalization,
before Terraform or deployment health checks, including when reusing an already
published image. Deployment failure does not restore deleted tags. An existing
preview using an older image may therefore fail to pull it on pod restart.
The cleanup matcher also recognizes legacy `pr-<number>-<SHA>` and `<number>-<SHA>` image tags
with eight-character or full SHAs. Other PR numbers never match.

On merge, read the greatest stable semantic Git tag reachable from the current
release branch (both `1.2.3` and `v1.2.3` are accepted). Ignore prereleases and
tags on unrelated branches. With no matching version, start from `0.0.0`.

| Current | major | minor | patch |
| --- | --- | --- | --- |
| 0.0.0 | 1.0.0 | 0.1.0 | 0.0.1 |
| 1.0.2 | 2.0.0 | 1.1.0 | 1.0.3 |
| 2.4.5 | 3.0.0 | 2.5.0 | 2.4.6 |

Publish the image as `<registry>/<image>:1.2.3`, then create an annotated Git tag
`v1.2.3` on the exact merged commit—not a moving `main` checkout. No `latest`
image tag is created. The annotation records PR, image, source commit and digest
for retry recovery. After the version image and Git tag exist, delete all
temporary SHA tags for that PR, including images used by retained previews.
Other PRs and semantic-version tags are never cleanup candidates. The merged PR
title receives ` [image: 1.2.3]`, preserving its original text (truncated only to
respect GitHub's title limit). Retries replace this workflow's suffix, not append
duplicates. **Merging never deploys to an actual environment**, with or without
`combine-deploy`. GitHub must confirm the PR is merged and its release label must
match the merge event.

A rerun of a release whose Git tag was already recorded performs cleanup only:
it does not rebuild or overwrite the released image. A failed push may have
partially published a tag; the retained release lock prevents another release
from reusing that version until the same workflow run is rerun successfully.
The label on a merged PR must still match the single label captured in the merge
event; changing labels afterward is rejected.

### Inputs and secrets

| Input | Default | Purpose |
| --- | --- | --- |
| `image` | required | Lowercase `namespace/repository`, without registry/tag |
| `registry` | `docker.io` | Container registry hostname, optionally with port |
| `registry_adapter` | `dockerhub` | `dockerhub` or `custom` |
| `registry_cleanup_script` | empty | Trusted Go adapter source path for another registry |
| `release_branch` | `main` | Target/release branch |
| `context` | `.` | Build context relative to the application repository |
| `dockerfile` | `Dockerfile` | Dockerfile relative to the application repository |
| `platforms` | `linux/arm64` | Raspberry Pi aarch64; optionally `linux/amd64,linux/arm64` for both architectures |
| `runs_on` | `"ubuntu-latest"` | Runner labels expressed as JSON |

Pass `GH_TOKEN` for GitHub API operations and repository checkouts, plus
`registry_username` and `registry_password` as Actions secrets. For Docker
Hub, use a username and access token with repository read/write/delete access.
The workflow checks authentication and tag listing before publishing; listing
cannot prove delete permission, so verify your token's permissions separately.
The caller needs `contents: write` and `pull-requests: write` for commands, PR
feedback and the release-title update. GitHub tag rules must allow release tags,
and `ci-docker-lock/*` for merged image releases; PR operations
create no Git tags. No bypass credentials are added.
Self-hosted runners need Docker and network access to GitHub and the registry.
Use ephemeral runners for PR builds.

### Raspberry Pi architecture and build cache

The default target is `linux/arm64`, matching the worker's `aarch64` Ubuntu
kernel. `amd64` is a different architecture; there is no Docker `arm64d` target.
The host kernel version does not need to be embedded into the image. Base images,
downloaded binaries and any native dependencies must support ARM64. Dockerfiles
must not hardcode `linux/amd64` or cross-compile the application for the wrong
architecture. QEMU enables ARM64 builds on an AMD64 runner.

BuildKit imports/exports the GitHub Actions cache using the v2 cache API:

- `mode=max` exports reusable intermediate build layers, not just final layers.
- Cache export is best-effort (`ignore-error=true`): reservation conflicts,
  quota limits or cache service failures are reported by BuildKit but do not
  fail an otherwise successful image build/push. A later build may need to
  rebuild uncached layers. Image build and registry push failures remain fatal.
- Scope keys include registry, image, context, Dockerfile path and platforms.
- Each PR writes its own cache; releases write a separate release cache.
- PR builds can read their previous cache and the release cache when GitHub's
  branch/cache access rules permit. Comment-triggered runs execute on the default
  branch, but still write separate PR scopes. Releases do not import PR-scoped caches.
- Missing caches cause a normal full build. Cache export errors are not ignored.
- Deleting old PR image tags does not delete the independent build cache.

GitHub cache storage limits and eviction still apply. Keep credentials out of
Dockerfile layers and build arguments; use BuildKit secret mounts for any future
build-time secrets. Cache exports can contain intermediate build files, so never
copy secrets into the build context/layers. This does not cache the Go
controller or run application tests.

### Publication serialization and failure recovery

GitHub Actions concurrency serializes image and deployment jobs per repository,
without cancelling running jobs. GitHub may replace pending jobs in this group.
PR builds and previews create no Git lock, SHA-reservation or preview tags.
Legacy locks are checked and reported explicitly, not bypassed or automatically
deleted. Existing recovery locks must be reconciled before migration.

For merged image releases, an atomic temporary annotated
Git ref, `ci-docker-lock/<repository-hash>`, also protects recovery.
Git versions are repository-wide,
not an independent version counter for each image. Call once per image/release
stream; this is not a coordinated multi-image release workflow.
Publishers wait up to 30 minutes for the lock, then fail explicitly.
Lock order is acquisition order, not guaranteed PR merge order.

Successful release runs release their Git lock;
failed releases deliberately retain it to prevent version reuse. **Rerun the
same failed workflow run**, not a new manually triggered run, to recover.
If the Git tag was recorded, rerun skips rebuilding and retries cleanup.
If publication or tag creation failed earlier, rerun retries that version while
holding the lock.

Abrupt runner termination/cancellation can leave a stale lock. Inspect its tag
annotation and owning run before deleting that **specific lock ref** manually;
never remove an active publisher's lock. If a release image exists without its
Git tag, recover the failed run before unlocking. Deletion errors are retried
three times, verified by listing, then fail the job explicitly. Old SHA tags
may remain until a successful retry; the workflow does not claim cleanup
succeeded when the registry denied it.

### Other registries, including Artifactory

Build/push supports a configurable registry hostname. **Only Docker Hub cleanup
is built in.** Other registries must supply their own tag-safe adapter; the
workflow fails rather than guessing a destructive digest-delete API.

```yaml
with:
  image: docker-local/team/application
  registry: registry.example.com
  registry_adapter: custom
  registry_cleanup_script: .github/registry-cleanup.go
```

The adapter is loaded from a separate checkout of the protected `release_branch`,
never from PR code. Commit it there before enabling the caller. It must be a
Go command (`package main`) supporting these invocations:

- `go run registry-cleanup.go list`: print only a JSON array of tag-name strings
  to stdout, including `[]` when empty.
- `go run registry-cleanup.go delete`: delete only the tag in `CLEANUP_TAG`.
  Exit nonzero on errors other than the tag already being absent.

Configuration is supplied through `IMAGE_REGISTRY`, `IMAGE_REPOSITORY`,
`REGISTRY_USERNAME`, and `REGISTRY_PASSWORD` environment variables, never command
arguments. The command runs from its source directory with a two-minute timeout.
Keep it self-contained or provide its Go module dependencies in that directory.
Its output is withheld on failure to avoid leaking credentials.

This is an adapter interface, not a working Artifactory implementation.
Implement API calls for the configured registry/version, use timeouts, and never
log credentials. The controller limits deletion candidates to this PR's exact
temporary-tag format and verifies absence afterward.
The adapter **must not delete a manifest/image version by digest** when that
would remove other tags. Version and SHA tags can reference the same digest.
Docker Hub uses its tag-specific deletion endpoint; released tags stay intact.
Removing registry tags does not guarantee immediate garbage collection of
unreferenced layer bytes; that is the registry's responsibility.

### Local checks

No test suite is included for this workflow, as requested. Any future tests
should be written in Go. The release controller is implemented entirely in
[Go](./.github/actions/docker-release/main.go), using the standard library for release control and HashiCorp's HCL parser for Terraform variable files.
The composite action installs Go 1.26 and invokes it for each operation; the
workflow wrapper remains GitHub Actions YAML. No custom JavaScript is required.

Format the controller without executing it:

```sh
gofmt -w .github/actions/docker-release/main.go
```

Go compilation, Go formatting, YAML syntax and Actions expression checks were
performed. No tests or Docker builds were run.
No images were published, registry tags deleted or GitHub refs created by this
local work. Actual end-to-end publication and cleanup remain unverified.

The reusable workflow references its action at `@main`, matching this
repository's existing convention. Pin both workflow and internal action
references to a reviewed commit/release for production. Publish these files in
the workflow repository before enabling caller workflows.

## CraftedBeyond combined application workflow

Use [combine-workflow.yml](./.github/workflows/combine-workflow.yml) with the
[application caller example](./examples/combine-workflow-caller.yml). It reuses the
Docker workflow above and one trusted `applications/<repository-name>/` root in
`Crafted-Beyond/crb-craftedbeyond-k3s-terraform`. Publish both repositories'
changes before enabling the caller. It does not install Argo CD, Gateway API,
Istio, DNS infrastructure or cluster controllers.

Application-owned Terraform roots are also supported: set
`infrastructure_repository` to the application repository,
`infrastructure_ref: main` and `infrastructure_directory: infra`. That root calls
the shared combine module through its Git source. `GH_TOKEN` authenticates module
downloads through a temporary Git configuration removed during cleanup.
When `infrastructure_repository` equals the calling repository, previews
automatically check out the exact published image SHA for Terraform and its
var-file, ignoring `infrastructure_ref` for that preview only. Actual deployments
and separate infrastructure repositories retain the configured ref. WireGuard API
uses this application-owned layout with `infrastructure_directory: infra/api`.
Only authorize trusted PRs: PR Terraform executes with cluster, registry and
backend credentials. Orchestration actions and authorization remain on protected
branches; a PR-supplied controller is not executed.

Callers with prerequisite provisioning can pass `preview_sha`, the authorized
immutable PR SHA used by that job. The image controller rejects a changed PR head
before publication, preventing database infrastructure from one revision being
combined with an image/API from another. Leave it empty for image-only or release
operations. The `issue_comment` caller itself must first be bootstrapped on the
default branch, but application infrastructure changes need not be merged to
preview them.

### Three separate operations

1. **PR image only:** one release label and its matching new command build a SHA
   image. No deployment.
2. **PR preview:** `combine-deploy` plus exactly one release label and a new
   `/combine-deploy` comment build a SHA image and deploy it. `/major`, `/minor`
   or `/patch` alone do not deploy, even if `combine-deploy` is present.
3. **Actual environment:** use **Run workflow** from the release branch, choose
   an environment and optionally supply `1.2.3` or `v1.2.3`. With no tag, select
   the greatest stable semantic Git tag reachable from the release branch and
   verify its corresponding registry image exists. This does not build an image.

Merge automatically publishes the version selected by the single release label;
no earlier command is required for the merge release. It updates the PR title
and **does not run Terraform or deploy**, regardless of `combine-deploy`.
Closing without merging does not build. Preview resources are kept after
merge/closure, but their images are not protected from publication cleanup.
No destroy command is added.

### Names, chart values and rollout

For repository `wireguard-api`, PR 42 uses namespace, project, Application and
Helm release `wireguard-api-pr42`; its hostname is
`wireguard-api-pr42.<domain>`. The chart derives application Deployment, Service
and HTTPRoute names from that release. The Terraform combine module validates
names; override its base `name` for long or non-DNS-safe repository names.

Actual environments default to the repository name for these resources. Use
separate cluster credentials for environments sharing these names. If dev/prod
share one cluster, select a distinct module `name` in HCL based on
`var.combine_runtime.environment` (for example `wireguard-api-dev`).
Domain and environment-specific application settings also belong in HCL. State is always
separate at `combine/<owner>/<repository>/<environment>/terraform.tfstate`;
previews use environment `pr42`.

Each central application root calls `module "combine"` and supplies native
`helm_values` for ports, resources, probes, configuration and workflow definitions.
Chart revision/path, domain, Argo CD namespace and optional application base name
are Terraform module arguments, not workflow inputs. For routed applications,
enable `helm_values.httpRoute` and configure its Gateway `parentRefs` in HCL.
Workflow-only applications can omit routing entirely.
The workflow passes a `combine_runtime` object containing only repository,
environment, PR number, preview flag, full SHA, registry and immutable image/tag.
Credentials are passed through secret environment variables, not application JSON.
The module authoritatively sets the image, release fullname, image-pull Secret
and optional hostname. Preview values cannot contain a fixed service-account name,
extra Services or explicit HTTPRoute backendRefs that bypass release-derived
naming. Use the chart's default backend for a preview.
Each root must export `combine_deployment = module.combine.deployment` as an
output; the workflow reads Terraform's application identity and route settings
for health checks and reporting. See the central repository's
`applications/example/main.tf` for a complete native HCL module call.

Terraform creates the namespace, image-pull Secret, project-scoped HTTPS Git chart
repository Secret, AppProject and Application using the existing registration
module. Argo CD deploys chart resources with automated sync/self-heal and **no
automatic prune**. Namespace destruction is blocked by default (callers with
explicit PR cleanup can opt out for previews only), and Applications have no
cascading deletion finalizer. Existing resources owned by another state require
a deliberate migration/import, not a second owner.

Deployment waits for a fresh Argo CD reconciliation, matching requested source,
successful sync operation and healthy application. When routing is enabled it requires HTTPRoute
`Accepted` and `ResolvedRefs` for the current generation. Gateway listeners must
allow routes from the preview namespaces; DNS/wildcard DNS and TLS must already
route these hostnames to that Gateway. This checks controller readiness, not an
external HTTP smoke test.

Older same-PR image tags are deleted during image publication, independently of
deployment state or health. Legacy `ci-preview/*` Git metadata no longer protects
images from cleanup. Label removal alone does not trigger cleanup.

### Required setup

| Configuration | Purpose |
| --- | --- |
| `image` | Existing registry namespace/repository |
| `state_repository` / `state_ref` | Existing **private** Git repository/branch for state |
| `deploy_runs_on` | Runner as JSON; default `"ubuntu-latest"` |
| `kubectl_version` | Installed kubectl version; default `v1.34.0`, must be within one minor of K3s |
| `infrastructure_repository` / `infrastructure_ref` | Infrastructure source; same-repository previews use the image SHA, otherwise the configured ref (default shared Terraform repository at `main`) |
| `preview_sha` | Optional authorized prerequisite PR commit; fail if the PR head changed before image preparation |
| `infrastructure_directory` | Central Terraform application root; default `applications/<calling-repository-name>` |
| `deployment_var_file` | Optional relative HCL `.tfvars` or JSON `.tfvars.json` path in that root, for example `environments/prod.tfvars`; supplies the actual environment and Terraform application settings |
| `environment` / `tag` | Explicit workflow_dispatch target and optional version |
| `tofu_version` | OpenTofu version; default `latest` |

Docker inputs (`registry`, adapter, context, Dockerfile, platforms) retain their
existing behavior. Use `build_runs_on` for build-runner labels.
When `deployment_var_file` is set, it is read from the selected infrastructure checkout before
loading state and passed to `tofu plan`. Its `environment` must match the manual
GitHub environment; preview state still uses its isolated `pr<number>` name.

Combined API deployments reuse `tofu-plan`, `tofu-apply` and `pr-comment` for
the same action-count tables, grouped details, job summaries and updatable
per-stack PR comments as the general OpenTofu workflow. Apply results and
state-save status are appended to the API comment. Partial applies still save
state before releasing the deployment lock.
Controller-driven database lifecycles can reuse
`.github/actions/tofu-plan/lifecycle.sh` with `plan` and `apply` phases. The
script uses the existing `summary.sh`, `log.sh` and `changes.jq`, supports a
var-file and destroy plan, and applies only the saved `lifecycle.tfplan`.
The controller remains responsible for state persistence, locks and publishing
the generated per-stack Markdown with `pr-comment`. Temporary raw JSON and
text inputs are private and removed when each phase finishes.

| Secret | Purpose |
| --- | --- |
| `registry_username`, `registry_password` | Build/push/list/delete credentials; required |
| `kubeconfig` | Selected cluster access; required for deployment |
| `chart_repository_url` | HTTPS chart Git URL on the same GitHub server, required for deployment; no embedded credentials |
| `GH_TOKEN` | GitHub API, application/infrastructure checkouts, private state read/write and chart repository read access |
| `image_pull_username`, `image_pull_password` | Optional paired read-only registry credentials for K3s; otherwise publisher credentials are used |
| `application_secrets` | Optional JSON object exported as `TF_VAR_application_secrets`; the central application root defines its required credential fields |

The example maps uppercase application secrets to these reusable secret names.
Use the existing repository/organization `GH_TOKEN` secret for all GitHub access;
Argo CD receives it as the chart repository password with username
`x-access-token`. The token must have access to every selected repository,
contents write for application tags/state, pull-request write for titles and
comments, and permission to read collaborator access. GitHub Actions `permissions`
does not grant scopes to a personal access token; configure those on the token.
The chart token is stored in a Kubernetes Secret and in deployment state.
For environment-specific overrides, configure the **reusable secret names** in
the GitHub environments (`preview`, `dev`, etc.). Protect actual environments
with reviewers as appropriate. Build credentials are repository secrets; preview
deployments select the `preview` environment. Keep runner access restricted to
trusted repositories, use ephemeral builders, and never run PR code on the
deployment runner. GitHub-hosted Ubuntu supplies Git and Docker. Setup actions
install Go, QEMU/Buildx, kubectl and OpenTofu for the jobs that need them.
K3s must be reachable from the hosted runner through a TLS-valid kubeconfig
endpoint or an explicitly configured private-network connection. No VPN,
firewall opening or TLS bypass is silently configured. Cluster requests have a
30-second preflight timeout.

Deployment uses the supplied kubeconfig directly. No SSH login, Cloudflare SSH
forward, or connection helper is used. Its server endpoint must be reachable
from the deployment runner with valid TLS and Kubernetes authentication.

Always-run cleanup removes generated deployment credentials, tfvars, plans,
working state and checkouts once state is persisted, and removes isolated Docker
credentials/build checkouts. Always-run steps remove this job's builder and log
out before deleting its Docker configuration.
Installed tools and remaining temporary files disappear with the hosted VM.
The GitHub Actions BuildKit cache is deliberately retained. No global Docker
prune, deletion of release images, or deletion of deployed resources is performed.

State and saved plans are not encrypted and can contain registry/chart
credentials. Keep the state repository private and restrict repository/runner
access. State is not uploaded as an Actions artifact.
Do not put secrets in chart values: Argo CD Application values
are readable through the Kubernetes API. Prefer pre-existing application Secrets
and chart Secret references.

### Failure recovery and serialization

Deployment shares the repository's Actions concurrency group with publication.
Both preview and actual-environment deployments acquire a recovery lock file in
the private state repository on `state_ref`, never a tag in the application repository.
Publication cleanup does not
wait for the replacement deployment to start or become healthy. Deployment jobs also
serialize per repository; GitHub may replace pending jobs in that group, but
never cancels the running deployment.

State is shared by PR number, never by commit SHA. For example, every PR #1 run
loads and updates `combine/Crafted-Beyond/wireguard-api/pr1/terraform.tfstate`.
Its lock is the sibling `deployment-lock.json`, recording the owning run, PR,
image repository and deployment mode. Creating that file without an existing
file SHA atomically acquires ownership; concurrent attempts fail instead of
overwriting it. Release uses the exact file SHA and does not remove state.

State is pushed even after a partial apply. If pushing fails, the backend lock
stays in place. A new run (including a retry of the same run) fails explicitly
with the owning run and lock path rather than taking over.
To recover: stop waiting runs, restore/import and verify the complete state at
the same backend path, then delete only that stack's `deployment-lock.json`
on `state_ref` after confirming the owning run is no longer active. Start a
new deployment; it acquires a new lock and loads the same saved PR state.
Do not delete `terraform.tfstate`, its backup or the stack directory to unlock.
If there is a legacy `ci-docker-lock/*` application tag, reconcile its owning
run's state before removing that specific legacy tag as a separate migration.
Inspect the failed run, restore the runner's local state to its exact private
state path, then rerun the **failed deployment job**, not the whole image build.
Private module Git authentication is scoped to the initialize/validate shell
step, not exported through `GITHUB_ENV`. Backend checkout supplies its own
authentication; combining both configurations can send duplicate Authorization
headers and reject state pushes.
Never unlock and start from stale/empty state after a partial apply.
On a state push failure, local state/backup are
left in the selected `infrastructure/applications/<repository-name>` root for manual recovery rather than deleted;
other credential-bearing files are still cleaned.
On GitHub-hosted runners, retaining files does not preserve them after the VM
is discarded. If the backend never received the state and the original file is
unavailable, reconstruct ownership by importing the existing resources into the
correct stack/state before any new apply. Review resource-specific import
requirements; do not delete the deployed namespace or database volume to retry.
Preview image cleanup already runs during publication; subsequent state-persistence
or health-check failures do not retain or restore superseded SHA tags.
If the runner is abruptly terminated before state can be saved, recover its
state before deleting its specific backend lock file; ephemeral runners cannot guarantee
recovery after an ungraceful crash. The Git state backend has no distributed
locking outside this workflow: do not apply the same stack elsewhere.

No tests were added. Go compilation, workflow schema/expression checks and
OpenTofu configuration validation do not prove live registry, Kubernetes, DNS,
or Argo CD behavior. No images, resources or state were published locally.

## OpenTofu CI/CD with a Git state backend

For on-prem stacks that cannot use S3 or another cloud backend. OpenTofu state is stored **in a Git repository**,
one directory per stack, instead of in cloud storage.

```
.github/actions/     small composite actions, each does one job
.github/workflows/   reusable workflow (tofu.yml) that chains the actions
```

| Action | What it does |
| --- | --- |
| `setup-tofu` | Installs OpenTofu |
| `load-env` | Exports `KEY=VALUE` secrets (for example `TF_VAR_*`) to the job and masks them |
| `state-pull` | Checks out the state repo and copies `<state_path>/terraform.tfstate` into the working directory |
| `tofu-validate` | `tofu fmt -check` and `tofu validate` (no backend, no state), with a result table in the job summary |
| `tofu-plan` | `init`, `validate`, `plan`, then writes the table and collapsible summary |
| `tofu-apply` | Applies the saved plan and writes an apply summary |
| `state-push` | Commits `terraform.tfstate` and `terraform.tfstate.backup` (the previous state) to the state repo, retrying on a concurrent push. Git history keeps every earlier version too |
| `pr-comment` | Creates or updates one plan comment per stack on the pull request |
| `cache-providers` | Caches provider plugins only (key from `.terraform.lock.hcl`). Saved by the automatic `Post Cache` step |
| `cleanup` | Always runs last: strips the token from git config, deletes the state checkout, state, plan files and `.terraform` |

### Cache and secret safety

- Only provider binaries are cached. State, plans and secrets are never cached.
- Commit `.terraform.lock.hcl` so the cache key is stable.
- The plan job checks out with read-only, non-persisted credentials. Only the apply job keeps the state token, and only
  until `cleanup` removes it.
- Secrets from `tofu_env` are masked in logs and only live in the job environment.

### How it runs

| Trigger in the caller | What happens |
| --- | --- |
| `pull_request` | Plan only. The plan is posted as a PR comment (when `comment_on_pr` is `true`, the default) |
| `push` / `workflow_dispatch` on `apply_branch` | Plan, then a **fresh plan** is applied and the state is pushed to the state repo |
| Any other branch | Plan only, never applies |

The PR comment and the job summary show a plan summary table (create, update, replace and delete counts), a collapsible
list of resources per action with the changed attribute names (never values), and the full plan output.
The apply job shows the result line, the apply log and the outputs.

### Use it from another repository

```yaml
name: Infrastructure
on:
  pull_request:          # remove this trigger to disable PR plans and comments
    paths:
      - repositories/**
  push:
    branches:
      - main
    paths:
      - repositories/**
  workflow_dispatch:

permissions:
  contents: read
  pull-requests: write

jobs:
  tofu:
    uses: Crafted-Beyond/github-action-workflow/.github/workflows/tofu.yml@main
    with:
      state_repository: Crafted-Beyond/github-terraform-backend
      state_path: github-terraform-resource      # folder in the state repo, defaults to this repo's name
      working_directory: repositories
      runs_on: '["self-hosted","linux"]'         # on-prem runner labels
      environment: prod                          # optional, adds a manual approval gate to apply
    secrets:
      state_repo_token: ${{ secrets.STATE_REPO_TOKEN }}
      tofu_env: |
        TF_VAR_github_token=${{ secrets.GH_TOKEN }}
```

| Input | Default | Purpose |
| --- | --- | --- |
| `state_repository` | required | State repo as `owner/name` |
| `state_path` | calling repo name | Directory in the state repo for this stack |
| `state_ref` | `main` | State repo branch |
| `working_directory` | `.` | Folder with the `.tf` files |
| `var_file` | none | Optional `-var-file` |
| `tofu_version` | `latest` | OpenTofu version |
| `runs_on` | `"ubuntu-latest"` | Runner labels as JSON |
| `comment_on_pr` | `true` | Post the plan on pull requests |
| `action` | `apply` | `plan` runs Terraform Plan only and never applies. `apply` runs Terraform Plan, then Terraform Apply |
| `apply_branch` | `main` | Only this branch may apply |
| `environment` | none | GitHub environment for the apply job |

### Secrets

Either pass secrets explicitly (`state_repo_token`, `tofu_env`) or use `secrets: inherit` with a `GH_TOKEN` secret
defined in the calling repo or shared to it from the organization. `GH_TOKEN` is used for the state repo, and when
`github_token_env` is set (for example `TF_VAR_github_token`) it is also exported under that name for the provider.

```yaml
    with:
      github_token_env: TF_VAR_github_token
    secrets: inherit
```

### Multiple environments (dev, test, preprod, prod)

The `environment` input gives each environment its own state folder in the state repo:

```
github-terraform-backend (branch: main)
└── my-stack/
    ├── dev/      terraform.tfstate + terraform.tfstate.backup
    ├── test/
    ├── preprod/
    └── prod/
```

It also selects the GitHub environment for the apply job, so `prod` can require manual approval. Call the workflow once per environment:

```yaml
jobs:
  dev:
    uses: Crafted-Beyond/github-action-workflow/.github/workflows/tofu.yml@main
    with:
      state_repository: Crafted-Beyond/github-terraform-backend
      environment: dev
      var_file: envs/dev.tfvars
    secrets:
      state_repo_token: ${{ secrets.GH_TOKEN }}
      tofu_env: |
        TF_VAR_github_token=${{ secrets.GH_TOKEN }}
  prod:
    needs: dev   # promote in order; add test and preprod the same way
    uses: Crafted-Beyond/github-action-workflow/.github/workflows/tofu.yml@main
    with:
      state_repository: Crafted-Beyond/github-terraform-backend
      environment: prod
      var_file: envs/prod.tfvars
    secrets:
      state_repo_token: ${{ secrets.GH_TOKEN }}
      tofu_env: |
        TF_VAR_github_token=${{ secrets.GH_TOKEN }}
```

| Environment | Branch | Plan | Apply |
| --- | --- | --- | --- |
| `dev` | any, including feature branches | yes | yes |
| `test` | `main` only | yes | yes, no approval |
| `preprod`, `prod` | `main` only | yes, no approval | waits for manual approval |

Other branches are skipped for `test`, `preprod` and `prod`. Pull requests that target `main` still get a plan-only
preview. `unrestricted_environments` (default `dev`) controls which environments may run from any branch.

Approval is configured on the GitHub side: under Settings, Environments in the calling repo, create `dev`, `test`,
`preprod` and `prod`, and add **Required reviewers** to `preprod` and `prod` only. Also set their deployment branches to
`main` (`dev`: all branches) as a second guard. The approval gate applies only to the apply job, so plans never wait. Without `environment`, state goes to `<state_path>/` as before.

### Requirements

- **State repo**: private, with `track_tfstate = true` in `github-terraform-resource` so `*.tfstate*` is not ignored.
  State can contain secrets, so restrict access to it.
- **Token**: `state_repo_token` needs read and write access to the state repo. If the state repo's default branch is
  protected, the token's owner must be allowed to push directly.
- **Backend**: the stack must use the default `local` backend (no `backend` block). The actions copy the state file in
  and out of the working directory.
- **Runners**: `bash`, `git`, `jq` and `node` (for `actions/*`). `tofu` is installed by `setup-tofu`.
- **Sharing**: this repo must allow access from other repos (Settings, Actions, Access), because they call its actions.
- **Pinning**: the workflow references its actions at `@main`. Pin the workflow and the actions to a tag or SHA
  for production use.
- **Forks**: pull requests from forks are skipped because they do not receive secrets.
- **Concurrency**: runs for the same state path are queued, so two applies never write the same state at once.
