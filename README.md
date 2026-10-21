# github-action-workflow

GitHub Actions workflow repository.

## Docker image builds and semantic releases

Reusable workflow: [docker-image.yml](./.github/workflows/docker-image.yml).
Copy the [caller example](./examples/docker-image-caller.yml) into an application's
`.github/workflows/` directory and replace its image name. Docker Hub is the
default; create the Docker Hub repository first.

The caller must include these PR event types:

```yaml
on:
  pull_request:
    branches: [main]
    types: [opened, reopened, synchronize, labeled, unlabeled, closed]
```

Do not use `pull_request_target`. Forks are skipped. Same-repository PR authors
and anyone allowed to assign release labels must be trusted: the Dockerfile
builds before merge with access to a registry-authenticated builder. Use a
dedicated narrowly scoped registry token, protect the release branch, and
restrict workflow/label changes. This is not an untrusted public-PR build service.

### Labels and tags

| PR event/state | Result |
| --- | --- |
| Open/reopen/new commit, no release label | Skip image build |
| Exactly one `major`, `minor` or `patch` label | Build current PR **head** commit |
| Release label added | Build current PR head commit |
| Unrelated label added | No build |
| Label removed | No build; still reject multiple remaining release labels |
| Two or more release labels | Fail before building |
| Closed without merge | No release |
| Merged into `main` with exactly one release label | Build exact merged commit and publish semantic version |

Temporary image tags are `pr-<PR-number>-<full-40-character-commit-SHA>`.
The PR prefix is deliberate: bare SHA tags can be shared by different PRs,
making safe PR-specific deletion impossible. These tags never contain a
`major`/`minor`/`patch` version. Outdated queued PR head events are skipped.
Changing one release label to another for the same SHA reuses that PR/SHA tag.
Removing the last label does not remove existing images or start a new build.

After a successful PR publication and verification that its new tag exists,
delete all older temporary tags **for that PR only**, keeping the current one.
Old images are not deleted before their replacement is published. Cleanup
also retries tags left by earlier failed cleanup, rather than only deleting
one remembered previous tag.

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
temporary full-SHA tags for that PR. Other PRs and semantic-version tags are
never cleanup candidates.

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

Pass `registry_username` and `registry_password` as Actions secrets. For Docker
Hub, use a username and access token with repository read/write/delete access.
The workflow checks authentication and tag listing before publishing; listing
cannot prove delete permission, so verify your token's permissions separately.
The caller needs `contents: write` and `pull-requests: read`. GitHub tag rules
must allow release tags and `ci-docker-lock/*`; no bypass credentials are added.
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
- Scope keys include registry, image, context, Dockerfile path and platforms.
- Each PR writes its own cache; releases write a separate release cache.
- PR builds can read their previous cache and the release cache when GitHub's
  branch/cache access rules permit. Releases do not import PR-scoped caches.
- Missing caches cause a normal full build. Cache export errors are not ignored.
- Deleting old PR image tags does not delete the independent build cache.

GitHub cache storage limits and eviction still apply. Keep credentials out of
Dockerfile layers and build arguments; use BuildKit secret mounts for any future
build-time secrets. Cache exports can contain intermediate build files, so never
copy secrets into the build context/layers. This does not cache the Go
controller or run application tests.

### Publication serialization and failure recovery

An atomic temporary annotated Git ref, `ci-docker-lock/<repository-hash>`,
serializes publications across this repository. Git versions are repository-wide,
not an independent version counter for each image. Call once per image/release
stream; this is not a coordinated multi-image release workflow.
Unlike Actions concurrency groups, it does not replace pending merge events.
Do not add caller concurrency rules that cancel/replace queued release runs.
Publishers wait up to 30 minutes for the lock, then fail explicitly.
Lock order is acquisition order, not guaranteed PR merge order.

Successful runs release their lock. Failed PR builds normally release it;
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
[Go](./.github/actions/docker-release/main.go), using only the standard library.
The composite action installs Go 1.26 and invokes it for each operation; the
workflow wrapper remains GitHub Actions YAML. No custom JavaScript is required.

Format the controller without executing it:

```sh
gofmt -w .github/actions/docker-release/main.go
```

Go formatting and YAML syntax checks were performed, but no tests or builds were run.
No images were published, registry tags deleted or GitHub refs created by this
local work. Actual end-to-end publication and cleanup remain unverified.

The reusable workflow references its action at `@main`, matching this
repository's existing convention. Pin both workflow and internal action
references to a reviewed commit/release for production. Publish these files in
the workflow repository before enabling caller workflows.

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
