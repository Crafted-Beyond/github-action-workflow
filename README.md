# github-action-workflow

GitHub Actions workflow repository.

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
