#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Offline stack/chart and image-probe contract tests. Images are simulated;
# qualify-openbao-ui-jwt-version checks selected published images separately.
set -euo pipefail
stack_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
repo_root=$(cd "$stack_dir/../../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail() { echo "openbao-ui-jwt-version-wiring: $*" >&2; exit 1; }
cp -R "$stack_dir" "$work/stack"
mkdir -p "$work/bin" "$work/image/openbao/plugins" "$work/image/app/migrations"
cp -R "$repo_root/migrations/openbao/migrations/utils" "$work/image/app/migrations/"
cp "$repo_root/infra/openbao/jwt-plugin-versions.env" "$work/image/openbao/"
export TEST_IMAGE_ROOT="$work/image" TEST_DOCKER_CALLS="$work/docker-calls"
export TEST_MIGRATION_COMPATIBLE=true
export DOCKER_CONFIG="$work/operator-docker"
mkdir -p "$DOCKER_CONFIG"
printf '{"credsStore":"fixture","auths":{"mirror.example.com":{}}}\n' > "$DOCKER_CONFIG/config.json"
cp "$DOCKER_CONFIG/config.json" "$work/original-docker.json"
export TEST_OPERATOR_DOCKER_CONFIG=$DOCKER_CONFIG
# shellcheck disable=SC1091
source "$work/image/openbao/jwt-plugin-versions.env"
target=$JWT_PLUGIN_TARGET_VERSION old=$JWT_PLUGIN_OLD_VERSION
touch "$work/image/openbao/plugins/vault-plugin-secrets-jwt-$target"
chmod +x "$work/image/openbao/plugins/vault-plugin-secrets-jwt-$target"

# Execute the actual probe bodies against fixture files, including the current
# migration helper. No Docker daemon or OpenBao server is used by these tests.
cat > "$work/bin/docker" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$TEST_DOCKER_CALLS"
if [[ $1 == login ]]; then
  supplied=$(cat)
  [[ $supplied == fixture-registry-key ]]
  [[ $DOCKER_CONFIG != "$TEST_OPERATOR_DOCKER_CONFIG" ]]
  yq -e '(.auths | has("nvcr.io")) and (.credsStore == null) and (.credHelpers == null)' "$DOCKER_CONFIG/config.json" >/dev/null
  printf '{"auths":{"nvcr.io":{"auth":"fixture"}}}\n' > "$DOCKER_CONFIG/config.json"
  exit
fi
[[ $1 == run ]]; shift
while (($#)); do
  case "$1" in
    --rm|--read-only) shift ;;
    --pull|--network|--cap-drop|--security-opt) shift 2 ;;
    --env) export "$2"; shift 2 ;;
    --entrypoint) entrypoint=$2; shift 2; break ;;
    *) exit 1 ;;
  esac
done
image=$1; shift
if [[ $image == nvcr.io/* && -n ${NVCF_RELEASE_NGC_DOCKER_CONFIG:-} ]]; then
  [[ $DOCKER_CONFIG == "$NVCF_RELEASE_NGC_DOCKER_CONFIG" ]]
  [[ -f $DOCKER_CONFIG/config.json ]]
else
  [[ $DOCKER_CONFIG == "$TEST_OPERATOR_DOCKER_CONFIG" ]]
fi
[[ $1 == -ec ]]; shift
probe=$1
if [[ $entrypoint == /bin/bash && $TEST_MIGRATION_COMPATIBLE == noop ]]; then
  exit 0
fi
if [[ $entrypoint == /bin/bash && $TEST_MIGRATION_COMPATIBLE == joined ]]; then
  joined_helper='enable_secrets_mount() { bao "secrets enable -path=$1 -plugin-version=$BAO_JWT_PLUGIN_VERSION $2"; }'
  probe=${probe/source \/app\/migrations\/utils\/functions.sh/$joined_helper}
elif [[ $entrypoint == /bin/bash && $TEST_MIGRATION_COMPATIBLE != true ]]; then
  # An older helper accepts the input but creates an unversioned mount.
  legacy_helper='enable_secrets_mount() { bao secrets enable "-path=$1" "$2"; }'
  probe=${probe/source \/app\/migrations\/utils\/functions.sh/$legacy_helper}
fi
probe=${probe//\/openbao\//$TEST_IMAGE_ROOT/openbao/}
probe=${probe//\/app\//$TEST_IMAGE_ROOT/app/}
"$entrypoint" -ec "$probe"
BASH
chmod +x "$work/bin/docker"
export PATH="$work/bin:$PATH"
environment=ui-jwt-test
printf '{}\n' > "$work/stack/secrets/$environment-secrets.yaml"
checker="$repo_root/tools/ci/check-openbao-ui-jwt-version"
chart="$repo_root/src/uis/nvcf-ui/helm"

render() {
  local setting=$1
  cat > "$work/stack/environments/$environment.yaml" <<YAML
addons:
  nvcfUi:
    enabled: true
    openbaoMigrations:
$setting
YAML
  HELMFILE_ENV=$environment HELMFILE_CACHE_HOME="$work/helmfile-cache" \
    helmfile -f "$work/stack/helmfile.d/02-core.yaml.gotmpl" \
    --state-values-set ingress.gatewayApi.gateways.shared.name=shared-gw \
    --state-values-set ingress.gatewayApi.gateways.shared.namespace=envoy-gateway-system \
    --state-values-set ingress.gatewayApi.gateways.grpc.name=grpc-gw \
    --state-values-set ingress.gatewayApi.gateways.grpc.namespace=envoy-gateway-system \
    -l name=nvcf-ui write-values --output-file-template "$work/ui-values.yaml" >/dev/null
  helm template nvcf-ui "$chart" -n nvcf-ui -f "$work/ui-values.yaml" > "$work/ui.yaml"
}
check() { "$checker" "$work/server.yaml" "$work/ui.yaml"; }
reject() {
  if check > "$work/result" 2>&1; then fail "accepted $1"; fi
  grep -Fq "$2" "$work/result" || { cat "$work/result" >&2; fail "wrong error for $1"; }
}
# A rendered server fixture selects the image to inspect. Its version manifest
# is intentionally independent of the repository manifest after the drift case.
cat > "$work/server.yaml" <<'YAML'
kind: StatefulSet
spec:
  template:
    spec:
      containers:
        - name: openbao
          image: registry.example.com/selected/server:release
YAML

render "      jwtPluginVersion: $target"
test "$(yq -r '.nvcfUi.openbaoMigrations.jwtPluginVersion' "$work/ui-values.yaml")" = "$target"
check
grep -Fq 'registry.example.com/selected/server:release' "$TEST_DOCKER_CALLS"
ui_image=$(yq -rN 'select(.kind == "Job") | .spec.template.spec.containers[] | select(.name == "openbao-migrations") | .image' "$work/ui.yaml")
grep -Fq "$ui_image" "$TEST_DOCKER_CALLS"

# Exercise qualification orchestration with real Helmfile and Helm. Substitute
# local charts for downloads; keep the actual saved environment and UI chart.
export TEST_HELMFILE_BIN
TEST_HELMFILE_BIN=$(command -v helmfile)
export TEST_UI_CHART="$chart" TEST_SERVER_CHART="$work/server-chart"
export TEST_REAL_HELM TEST_AUTH_HELM="$work/auth-helm" TEST_AUTH_LOG="$work/auth-log"
TEST_REAL_HELM=$(command -v helm)
cat > "$TEST_AUTH_HELM" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == repo && $2 == add ]]; then
  if [[ $3 == nvcf ]]; then
    [[ $4 == https://helm.ngc.nvidia.com/nvidia/nvcf ]]
    [[ $* == *--password-stdin* && $* != *fixture-registry-key* ]]
    [[ $(cat) == fixture-registry-key ]]
    echo ngc-authenticated >> "$TEST_AUTH_LOG"
  fi
  exit 0
fi
exec "$TEST_REAL_HELM" "$@"
BASH
chmod +x "$TEST_AUTH_HELM"
mkdir -p "$TEST_SERVER_CHART/templates"
printf 'apiVersion: v2\nname: test-openbao\nversion: 0.0.0\n' > "$TEST_SERVER_CHART/Chart.yaml"
cp "$repo_root/deploy/helm/openbao/helm/values.yaml" "$TEST_SERVER_CHART/values.yaml"
cat > "$TEST_SERVER_CHART/templates/server.yaml" <<'YAML'
kind: StatefulSet
apiVersion: apps/v1
metadata:
  name: openbao-server
spec:
  template:
    spec:
      containers:
        - name: openbao
          image: "{{ .Values.openbao.server.image.registry }}/{{ .Values.openbao.server.image.repository }}:{{ .Values.openbao.server.image.tag }}"
YAML
cat > "$work/bin/helmfile" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
for arg in "$@"; do
  if [[ $arg == template ]]; then
    if [[ ${TEST_CHART_ORIGIN:-} == ngc ]]; then
      [[ $NVCF_USERNAME == '$oauthtoken' && $NVCF_PASSWORD == fixture-registry-key ]]
      auth_args=()
      for auth_arg in "$@"; do
        if [[ $auth_arg == template ]]; then auth_args+=(repos); else auth_args+=("$auth_arg"); fi
      done
      "$TEST_HELMFILE_BIN" --helm-binary "$TEST_AUTH_HELM" "${auth_args[@]}"
    else
      [[ ${NVCF_PASSWORD:-} != fixture-registry-key ]]
    fi
    if [[ $* == *01-dependencies.yaml.gotmpl* ]]; then
      exec "$TEST_HELMFILE_BIN" "$@" --chart "$TEST_SERVER_CHART" --skip-deps
    else
      exec "$TEST_HELMFILE_BIN" "$@" --chart "$TEST_UI_CHART" --skip-deps
    fi
  fi
done
exec "$TEST_HELMFILE_BIN" "$@"
BASH
chmod +x "$work/bin/helmfile"
qualify() {
  NVCF_RELEASE_NGC_API_KEY=${TEST_NGC_API_KEY:-} "$repo_root/tools/ci/qualify-openbao-ui-jwt-version" "$work/stack" "$environment" \
    --state-values-set ingress.gatewayApi.gateways.shared.name=shared-gw \
    --state-values-set ingress.gatewayApi.gateways.shared.namespace=envoy-gateway-system \
    --state-values-set ingress.gatewayApi.gateways.grpc.name=grpc-gw \
    --state-values-set ingress.gatewayApi.gateways.grpc.namespace=envoy-gateway-system "$@"
}
TEST_NGC_API_KEY=fixture-registry-key qualify
# Pin distinct fixture image references to detect incorrect release selection.
yq -i '.global.image.registry = "mirror.example.com" |
  .global.image.repository = "selected" |
  .addons.nvcfUi.openbaoMigrations.image.tag = "fixture-migration"' "$work/stack/environments/$environment.yaml"
: > "$TEST_DOCKER_CALLS"
qualify
server_tag=$(yq -r '.openbao.server.image.tag' "$TEST_SERVER_CHART/values.yaml")
grep -Fq "mirror.example.com/selected/nvcf-openbao:$server_tag" "$TEST_DOCKER_CALLS"
grep -Fq 'mirror.example.com/selected/nvcf-openbao-migrations:fixture-migration' "$TEST_DOCKER_CALLS"
# The NGC key must not replace mirror chart credentials or leak at debug level.
yq -i '.global.helm.sources.url = "https://mirror.example.com/charts" |
  .global.helm.sources.username = "mirror-user" |
  .global.helm.sources.password = "mirror-password"' "$work/stack/environments/$environment.yaml"
TEST_NGC_API_KEY=fixture-registry-key qualify --log-level debug > "$work/result" 2>&1
if grep -Fq fixture-registry-key "$work/result"; then fail 'NGC key leaked in mirror debug output'; fi
grep -Fq mirror-password "$work/result"
# An exact NGC origin may use the key without rendering it into Helmfile state.
TEST_CHART_ORIGIN=ngc TEST_NGC_API_KEY=fixture-registry-key qualify --log-level debug \
  --state-values-set global.helm.sources.url=https://helm.ngc.nvidia.com/nvidia/nvcf \
  --state-values-set global.helm.sources.username= \
  --state-values-set global.helm.sources.password= > "$work/result" 2>&1
if grep -Fq fixture-registry-key "$work/result"; then fail 'NGC key leaked in NGC debug output'; fi
test "$(grep -c ngc-authenticated "$TEST_AUTH_LOG")" = 2
# A lookalike hostname is still a mirror.
TEST_NGC_API_KEY=fixture-registry-key qualify \
  --state-values-set global.helm.sources.url=https://helm.ngc.nvidia.com.example.com/charts
cmp "$work/original-docker.json" "$DOCKER_CONFIG/config.json"
if grep -Fq fixture-registry-key "$TEST_DOCKER_CALLS"; then fail 'registry credential appeared in command arguments'; fi
render "      jwtPluginVersion: $old"
if qualify > "$work/result" 2>&1; then fail 'qualification accepted stale saved environment values'; fi
grep -Fq "expected $target" "$work/result"
rm "$work/bin/helmfile"

render '      image: {}'
reject 'missing version' 'set addons.nvcfUi.openbaoMigrations.jwtPluginVersion'
render '      jwtPluginVersion: ""'
reject 'empty version' 'missing or empty'
render "      jwtPluginVersion: $old"
reject 'old version' "expected $target"
render '      jwtPluginVersion: v99.0.0'
reject 'mismatched version' "expected $target"

render "      jwtPluginVersion: $target"
OPENBAO_UI_JWT_TEST_VERSION=$target yq -i '(select(.kind == "Job") | .spec.template.spec.containers[] | select(.name == "openbao-migrations") | .env[] | select(.name == "BAO_JWT_PLUGIN_VERSION") | .value) = strenv(OPENBAO_UI_JWT_TEST_VERSION) + "\n"' "$work/ui.yaml"
reject 'version with trailing newline' "expected $target"
render "      jwtPluginVersion: $target"
export TEST_MIGRATION_COMPATIBLE=joined
reject 'migration helper joining arguments' 'does not support explicit JWT plugin version selection'
export TEST_MIGRATION_COMPATIBLE=false
reject 'migration image that ignores the version' 'does not support explicit JWT plugin version selection'
export TEST_MIGRATION_COMPATIBLE=true

export TEST_MIGRATION_COMPATIBLE=noop
reject 'migration helper that never enables a mount' 'did not create the mount'
export TEST_MIGRATION_COMPATIBLE=true

yq -i 'del(.spec.template.spec.containers[].env[] | select(.name == "BAO_JWT_PLUGIN_VERSION"))' "$work/ui.yaml"
reject 'a chart that drops the configured version' 'set addons.nvcfUi.openbaoMigrations.jwtPluginVersion'
render "      jwtPluginVersion: $target"
yq -i '(select(.kind == "Job") | .spec.template.spec.containers[] | select(.name == "openbao-migrations") | .env) += [{"name": "BAO_JWT_PLUGIN_VERSION", "value": ""}]' "$work/ui.yaml"
reject 'duplicate version entries' 'exactly one BAO_JWT_PLUGIN_VERSION'
render "      jwtPluginVersion: $target"

# Detect drift in the selected image even when the checkout manifest is unchanged.
drift=v99.1.2
printf 'JWT_PLUGIN_TARGET_VERSION=%s\n' "$drift" > "$work/image/openbao/jwt-plugin-versions.env"
cp "$work/image/openbao/plugins/vault-plugin-secrets-jwt-$target" "$work/image/openbao/plugins/vault-plugin-secrets-jwt-$drift"
reject 'selected image manifest drift' "expected $drift"
render "      jwtPluginVersion: $drift"
check

# A target-only image with no readable manifest must not fall back to legacy.
mv "$work/image/openbao/jwt-plugin-versions.env" "$work/manifest.env"
reject 'missing image manifest' 'cannot inspect JWT plugin layout'
mv "$work/manifest.env" "$work/image/openbao/jwt-plugin-versions.env"

# Published legacy and bridge images retain the unversioned executable.
cp "$work/image/openbao/plugins/vault-plugin-secrets-jwt-$target" "$work/image/openbao/plugins/vault-plugin-secrets-jwt"
render '      image: {}'
check
mv "$work/image/openbao/jwt-plugin-versions.env" "$work/manifest.env"
check

# A disabled UI requires neither registry access nor images.
printf 'addons:\n  nvcfUi:\n    enabled: false\n' > "$work/stack/environments/$environment.yaml"
: > "$TEST_DOCKER_CALLS"
NVCF_RELEASE_NGC_API_KEY='' "$repo_root/tools/ci/qualify-openbao-ui-jwt-version" "$work/stack" "$environment"
test ! -s "$TEST_DOCKER_CALLS"

echo 'openbao-ui-jwt-version-wiring: all checks passed (simulated images)'
