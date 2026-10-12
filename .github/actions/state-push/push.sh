#!/usr/bin/env bash
# Copies the working directory's terraform.tfstate into the state repository, commits and pushes it.
# Env: STATE_PATH, CHECKOUT_PATH, WORKDIR, REF, MESSAGE, GIT_USER_NAME, GIT_USER_EMAIL
set -euo pipefail

case "$STATE_PATH" in
  "" | /* | *..*)
    echo "::error::state_path must be a relative directory without '..'."
    exit 1
    ;;
esac

src="$WORKDIR/terraform.tfstate"
if [ ! -f "$src" ]; then
  echo "No terraform.tfstate in $WORKDIR, nothing to save."
  exit 0
fi

dest="$CHECKOUT_PATH/$STATE_PATH"
mkdir -p "$dest"
cp "$src" "$dest/terraform.tfstate"

# OpenTofu writes the previous state to terraform.tfstate.backup on every apply that changes state.
files=("$STATE_PATH/terraform.tfstate")
if [ -f "$src.backup" ]; then
  cp "$src.backup" "$dest/terraform.tfstate.backup"
  files+=("$STATE_PATH/terraform.tfstate.backup")
fi

cd "$CHECKOUT_PATH"
git config user.name "$GIT_USER_NAME"
git config user.email "$GIT_USER_EMAIL"

# -f because state repositories often ignore *.tfstate*.
git add -f -- "${files[@]}"
if git diff --cached --quiet; then
  echo "State unchanged, nothing to commit."
  exit 0
fi

git commit -m "$MESSAGE"

for attempt in 1 2 3 4 5; do
  if git push origin "HEAD:$REF"; then
    echo "State saved to $STATE_PATH/terraform.tfstate"
    exit 0
  fi
  echo "Push rejected (attempt $attempt), rebasing on the remote and retrying."
  git pull --rebase origin "$REF"
  sleep $((attempt * 2))
done

echo "::error::Could not push the state to the state repository."
exit 1
