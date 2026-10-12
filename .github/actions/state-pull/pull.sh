#!/usr/bin/env bash
# Copies <checkout>/<state_path>/terraform.tfstate into the working directory.
# Env: STATE_PATH, CHECKOUT_PATH, WORKDIR
set -euo pipefail

case "$STATE_PATH" in
  "" | /* | *..*)
    echo "::error::state_path must be a relative directory without '..'."
    exit 1
    ;;
esac

src="$CHECKOUT_PATH/$STATE_PATH/terraform.tfstate"
if [ -f "$src" ]; then
  cp "$src" "$WORKDIR/terraform.tfstate"
  echo "Loaded state from $STATE_PATH/terraform.tfstate"
  echo "state_exists=true" >>"$GITHUB_OUTPUT"
else
  echo "No state found at $STATE_PATH/terraform.tfstate, starting from empty state."
  echo "state_exists=false" >>"$GITHUB_OUTPUT"
fi
