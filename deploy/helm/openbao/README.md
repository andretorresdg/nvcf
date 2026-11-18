# NVCF OpenBao Helm Chart

This repository contains an opinionated Helm chart for deploying OpenBao in the
NVIDIA Cloud Functions (NVCF) self-hosted environment.

## Overview

The chart is designed to provide:

- An OpenBao server deployment built on top of the upstream
  `openbao/openbao-helm` chart
- A bootstrap Job that initializes and unseals members before readiness
- Post-install JWT configuration and post-install/post-upgrade migrations
- Opinionated defaults for HA, injector enablement, and operational hooks

## Readiness and bootstrap

The server readiness probe accepts active and standby members after unseal.
Sealed and uninitialized members stay NotReady. Client Services must set
`openbao.server.service.publishNotReadyAddresses=false` to exclude them from
routing. The self-managed HA values set this flag; standalone installs must
set it explicitly. Keep the internal headless Service publishing NotReady
addresses so Raft peers can discover one another during bootstrap.

The chart creates a regular bootstrap Job with the StatefulSet. It uses each
member's local API while the member is NotReady, initializes the primary,
saves and verifies credentials, marks bootstrap complete and joins/unseals peers. Bootstrap requires
`openbao.server.podManagementPolicy=Parallel`; `OrderedReady` would prevent
successor Pods from starting before the primary is initialized.

With the default `OnDelete` strategy, Helm 3 `--wait` does not wait for the
StatefulSet's Pods to become Ready. Adding `--wait-for-jobs` waits for the
regular bootstrap Job. Independently of those flags, the post-install hook
waits for initialized, unsealed members and the saved root token before JWT
setup. A wait-only post-upgrade hook runs before catalog refresh and migrations.
These hooks never initialize members. `initializeCluster.schedulingGates`
applies only to the post-install JWT Job. An external controller must release
those gates; bootstrap and the upgrade barrier are ungated.

```mermaid
sequenceDiagram
    Helm->>Kubernetes: Create StatefulSet, bootstrap scripts and bootstrap Job
    Note over Kubernetes: Members expose APIs and remain NotReady
    Bootstrap->>OpenBao: Initialize primary and persist both credentials
    Bootstrap->>Kubernetes: Mark credential bootstrap complete
    Bootstrap->>OpenBao: Join and unseal members
    OpenBao->>Kubernetes: Members become Ready
    opt Helm --wait --wait-for-jobs
        Kubernetes->>Helm: Bootstrap Job completes
    end
    Helm->>Barrier: Run post-install or post-upgrade hook
    Barrier->>OpenBao: Wait for all current members
    Barrier->>Kubernetes: Check root token and bootstrap completion
    Barrier->>Helm: Continue JWT setup or catalog refresh and migrations
```

Bootstrap waits up to `openbao.hooks.initializeCluster.apiTimeoutSeconds`
(default 600 seconds) per API/bootstrap wait phase. Status calls have a
five-second cap. Credential persistence has a separate
`openbao.hooks.initializeCluster.credentialTimeoutSeconds` budget (default
120 seconds) per Secret operation. Standalone scripts accept the equivalent
`OPENBAO_CREDENTIAL_TIMEOUT_SECONDS` setting. Slow credential reads do not use
the status polling cap. It reads the StatefulSet replica count
and selects the `openbao` container by name. A new Job name on each release
revision avoids immutable Job-template errors on upgrade. Existing initialized
members reuse saved credentials. Inconsistent state or missing credentials
fails closed instead of initializing again; restore the matching data and
credentials before retrying. Secret patches include the version read, so a
concurrent restore causes a conflict and a reread instead of an overwrite.
The completion check also rereads the desired replica count before releasing
the next hook.

New root-token Secrets include `bootstrap_state=pending` until both bootstrap
credentials are verified. If recovery-key persistence fails in auto-unseal
mode, the saved root token remains available for recovery, but retries and
hooks reject the pending state. Restore the matching recovery-key Secret before
retrying; initialization is never repeated. A retry can mark pending state
complete after verifying both saved credentials. Once `bootstrap_state` is
`complete`, recovery keys can be exported and the recovery-key Secret deleted.
Do not delete the completion field from the root-token Secret. Resolve pending
credentials before rolling back to a chart that does not check this field.

Existing root-token Secrets without this field retain the legacy credential
checks. Before upgrading a previously failed auto-unseal installation, verify
that its recovery shares were saved or exported; an unsealed server and a root
token alone do not prove this. The chart cannot reconstruct lost recovery shares.

When upgrading with `--reuse-values`, explicitly replace the old readiness
path, which otherwise remains in the reused values:

```bash
helm upgrade openbao-server helm --namespace vault-system \
  --reuse-values \
  --set openbao.server.service.publishNotReadyAddresses=false \
  --set-string 'openbao.server.readinessProbe.path=/v1/sys/health?standbyok=true' \
  --wait --wait-for-jobs --timeout 20m
```

Retain your image, namespace and migration overrides when upgrading. With an
`OnDelete` StatefulSet strategy, the probe change reaches each Pod only after
that Pod is recreated. Roll one member at a time and verify recovery and Raft
quorum before continuing. Rollback also restores only the StatefulSet template.
After rolling back to an older chart, replace Pods one at a time to restore
that chart's probe on running members. Verify recovery and quorum before
replacing the next Pod.

### Upgrading an existing OrderedReady StatefulSet

`podManagementPolicy` cannot be changed in place. Keep the existing Pods,
PVCs and bootstrap Secrets while replacing only the StatefulSet controller.
Run this procedure from `deploy/helm/openbao` with the candidate dependencies
built. Use the original release, namespace and StatefulSet names. Do not combine
this procedure with image, storage, selector, Service name or replica changes.
Verify all members are initialized, unsealed and have Raft quorum first.

Save the current StatefulSet manifest and the Pod/PVC UIDs for comparison.
Render the candidate with the same overrides and check that selectors, Service
name and volume-claim templates still match. Keep existing image, migration
and JWT lifecycle settings through `--reuse-values` and your original overrides.

```bash
namespace=vault-system
release=openbao-server
statefulset=openbao-server
kubectl -n "$namespace" get statefulset "$statefulset" -o yaml > orderedready-statefulset.yaml
kubectl -n "$namespace" get pods -o json > orderedready-pods.json
kubectl -n "$namespace" get pvc -o json > orderedready-pvcs.json

kubectl -n "$namespace" patch statefulset "$statefulset" --type=merge \
  -p '{"spec":{"persistentVolumeClaimRetentionPolicy":{"whenDeleted":"Retain","whenScaled":"Retain"}}}'
```

Wait for any PVC owner references to the old StatefulSet or its Pods to be
removed by the retention-policy reconciliation. Then orphan its dependents.
Do not delete Pods, PVCs or Secrets.

```bash
kubectl -n "$namespace" delete statefulset "$statefulset" --cascade=orphan --wait=true
helm upgrade "$release" helm --namespace "$namespace" --reuse-values \
  --set openbao.server.podManagementPolicy=Parallel \
  --set openbao.server.updateStrategyType=OnDelete \
  --set openbao.server.persistentVolumeClaimRetentionPolicy.whenDeleted=Retain \
  --set openbao.server.persistentVolumeClaimRetentionPolicy.whenScaled=Retain \
  --set openbao.server.service.publishNotReadyAddresses=false \
  --set-string 'openbao.server.readinessProbe.path=/v1/sys/health?standbyok=true' \
  --wait --wait-for-jobs --timeout 20m
```

Confirm the replacement StatefulSet uses `Parallel` and has adopted the same
Pods. Their controller owner reference must use the replacement StatefulSet
UID. Compare Pod and PVC UIDs with the saved snapshots, verify quorum and check
that saved credentials and cluster identity are unchanged. `OnDelete` keeps
existing Pods running during adoption. Replace them one at a time later to
apply the strict probe, verifying recovery and quorum between replacements.
Keep PVC retention set to `Retain` during this procedure. Restore a different
retention or rollout policy only after adoption and recovery checks pass.

### Resuming a standalone installation

For a failed standalone installation whose data and saved bootstrap Secrets
are intact, resume against the existing resources from this directory:

```bash
bash deploy.sh vault-system openbao-server helm
```

Use the original namespace and StatefulSet name. This existing-resources mode
requires `kubectl`, `jq` and `jwker`, skips Helm installation and reuses saved
credentials before joining/unsealing remaining members and configuring JWT.
The default `script` mode remains for fresh installations. Missing credentials
or mismatched data require recovery before retrying; do not delete resources
as a routine retry step.

## JWT plugin lifecycle

The bridge server image supplies declarative plugin registration at
`/openbao/plugin-config`; its entrypoint loads that directory on server startup.
Set `openbao.jwtPluginLifecycle.mode` for the deployment stage:

- `legacy` (default): retains the refresh hook for published pre-bridge images.
  Preflight rejects versioned images before changing the release.
- `bridge`: checks existing mount compatibility without rewriting the catalog.
  Defers migrations. Keep hooks enabled and rotate every member before enabling
  versioned mount creation. Upgrades only.
- `versioned`: checks image compatibility and pins new signing mounts to the
  target version from the server image. Use for fresh bridge installations
  and after an existing deployment completes its bridge migration.

The lifecycle driver can check and tune mounts independently of Helm hooks.
The migration Job rechecks catalog readiness after rollout because the active
member can change after preflight. Its init container also supplies the server
image's version manifest to migrations.

Publish and qualify the matching bridge server and migrations images before
advancing chart or stack image pins. Existing deployments need the staged
[JWT plugin upgrade procedure](../../../docs/self-managed/runbooks/openbao-jwt-plugin-upgrade.md),
including the explicit old version and a later cleanup release.

## Prerequisites

- Kubernetes cluster compatible with the bundled upstream OpenBao chart
- Helm 3
- `kubectl` configured for the target cluster
- Access to the required container images

## Images

This repository is published as chart source only.

This chart depends on NVIDIA-managed container images for the OpenBao server
extensions, migration jobs, and related operational tooling.

The default chart values do not set the required image registries and
repositories for those components. They must be supplied through an additional
values file at install time, and access to those images must be arranged
separately.

Example:

```yaml
openbao:
  migrations:
    image:
      registry: nvcr.io
      repository: <your-org>/nvcf-openbao-migrations
  server:
    image:
      registry: nvcr.io
      repository: <your-org>/nvcf-openbao
  injector:
    image:
      registry: nvcr.io
      repository: <your-org>/oss-vault-k8s
    agentImage:
      registry: nvcr.io
      repository: <your-org>/nvcf-openbao
```

The injector Deployment uses `openbao.injector.image` (`oss-vault-k8s`). The
Vault Agent sidecars it creates use `openbao.injector.agentImage`. The
self-managed stack selects `nvcf-openbao` for both the agent and server; the
auto-unseal sidecar also uses the server image. The chart checks that their
tags match. The shipped stack selects no third-party Vault Agent image.
Standalone installs supply the registries and repositories shown above.
Operators who select a separate agent image own its toolchain and CA bundle
verification.

## Installation

This chart is intended to be installed by invoking Helm directly from source.

1. Build dependencies:

```bash
helm dependency build helm
```

1. Install the chart:

```bash
helm install openbao-server helm \
  --namespace vault-system \
  --create-namespace \
  --values helm/values.yaml \
  --values path/to/values.yaml \
  --set openbao.server.service.publishNotReadyAddresses=false \
  --wait \
  --wait-for-jobs \
  --timeout 20m
```

## Configuration

Chart configuration lives in [helm/values.yaml](./helm/values.yaml). Additional
values files can be layered on top during installation.

Key configurable areas include:

- OpenBao server image settings and HA configuration
- Migration job image settings and migration environment variables
- Injector image settings
- Image pull secrets
- OIDC issuer discovery for migration-time JWT auth configuration

Important note on Helm list merging:

When overriding list values such as `extraContainers`, Helm replaces the
original list instead of merging it. Override the full list item to preserve
required fields from the base values file.

### Migration kubectl client

`openbao.migrations.kubectlMinor` controls both the initialization and migration
Jobs. Leave it empty to use the image default, or set `"1.36"` for Kubernetes
1.35 servers. The migrations 0.23.1 image provides kubectl 1.37.2 by default
and the patched 1.36.6 client through `NVCF_KUBECTL_MINOR="1.36"`. An explicit
`"1.37"` is also supported; other selections fail rendering.

Use an image that implements `NVCF_KUBECTL_MINOR` when setting this value.
Configure the selector here rather than also setting it in
`openbao.migrations.env`; specifying both fails rendering. Existing env-only
configuration still applies to the migration Job when `kubectlMinor` is empty.

## Uninstallation

```bash
helm uninstall openbao-server --namespace vault-system
```

## License

This project is licensed under the
[Apache License, Version 2.0](https://www.apache.org/licenses/LICENSE-2.0).
