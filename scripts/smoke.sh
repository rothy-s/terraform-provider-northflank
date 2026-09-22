#!/usr/bin/env bash
#
# Offline smoke test.
#
# Builds the provider, points Terraform at the local binary with a
# dev_overrides block, and plans test/smoke against it. This runs the real
# plugin protocol -- schemas, plan modifiers, validators and sensitivity all
# get exercised the way Terraform will exercise them, which the Go unit tests
# cannot cover on their own.
#
# No credentials and no network access are needed: every resource in the
# config is new, and every data source depends on one, so Terraform defers the
# reads to apply. Nothing is ever applied.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
config_dir="$repo_root/test/smoke"

if ! command -v terraform >/dev/null 2>&1; then
	echo "smoke: terraform is not installed; skipping" >&2
	exit 0
fi

echo "smoke: building provider"
(cd "$repo_root" && go build -o terraform-provider-northflank .)

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

cat >"$workdir/dev.tfrc" <<TFRC
provider_installation {
  dev_overrides {
    "srs/northflank" = "$repo_root"
  }
  direct {}
}
TFRC

export TF_CLI_CONFIG_FILE="$workdir/dev.tfrc"
# Keep Terraform's scratch state out of the repo.
export TF_DATA_DIR="$workdir/.terraform"
# Required by the provider, never sent anywhere.
export NORTHFLANK_API_TOKEN="smoke-test-token-unused"

echo "smoke: terraform fmt"
(cd "$config_dir" && terraform fmt -check -recursive .)

# Plan from a copy rather than from test/smoke itself. Should anyone ever
# apply that config for real, the state it leaves behind must not turn this
# offline check into a refresh against the live API.
mkdir -p "$workdir/config"
cp "$config_dir"/*.tf "$workdir/config/"
cd "$workdir/config"

echo "smoke: terraform validate"
terraform validate >/dev/null

echo "smoke: terraform plan"
terraform plan -input=false -no-color -out="$workdir/plan.bin" >"$workdir/plan.txt" || {
	cat "$workdir/plan.txt" >&2
	echo "smoke: plan failed" >&2
	exit 1
}

terraform show -json "$workdir/plan.bin" >"$workdir/plan.json"

# Assert the plan actually covers every registered type, so that adding a
# resource without adding it here is caught rather than silently skipped.
python3 - "$workdir/plan.json" "$repo_root/internal/provider/provider.go" <<'PY'
import json
import re
import sys

plan_path, provider_path = sys.argv[1], sys.argv[2]

with open(plan_path) as handle:
    plan = json.load(handle)

planned = {change["type"] for change in plan.get("resource_changes", [])}

# Data sources appear in the planned state rather than in resource_changes,
# because their reads are deferred to apply.
root = plan.get("planned_values", {}).get("root_module", {})
read = {r["type"] for r in root.get("resources", []) if r.get("mode") == "data"}

# Derive the expected list from the provider's registration functions, so this
# check cannot drift out of date on its own.
source = open(provider_path).read()


def registered(block: str, suffix: str) -> set[str]:
    # Matches both an empty "{}" and a multi-line list of constructors.
    body = re.search(block + r"\{(.*?)\}\n", source, re.S)
    if not body:
        sys.exit(f"smoke: could not find the {suffix} registration block")
    names = re.findall(r"New(\w+?)" + suffix + r",", body.group(1))
    out = set()
    for name in names:
        # NewSecretGroupResource -> northflank_secret_group
        snake = re.sub(r"(?<!^)(?=[A-Z])", "_", name).lower()
        out.add("northflank_" + snake)
    return out


want_resources = registered(r"\[\]func\(\) resource\.Resource", "Resource")
want_data = registered(r"\[\]func\(\) datasource\.DataSource", "DataSource")

missing = want_resources - planned
if missing:
    sys.exit(
        "smoke: these resources are registered but absent from the plan: "
        + ", ".join(sorted(missing))
        + "\n       add them to test/smoke/main.tf"
    )

missing_data = want_data - read
if missing_data:
    sys.exit(
        "smoke: these data sources are registered but absent from the plan: "
        + ", ".join(sorted(missing_data))
        + "\n       add them to test/smoke/main.tf"
    )

# Secret variables must never surface in plan output.
for change in plan.get("resource_changes", []):
    if change["type"] != "northflank_secret_group":
        continue
    after_sensitive = change["change"].get("after_sensitive")
    if not (isinstance(after_sensitive, dict) and after_sensitive.get("variables")):
        sys.exit(f"smoke: {change['address']} does not mark `variables` as sensitive")

print(
    f"smoke: OK -- planned {len(planned)} resource types "
    f"({', '.join(sorted(planned)) or 'none registered yet'}) and {len(read)} data sources"
)
PY
