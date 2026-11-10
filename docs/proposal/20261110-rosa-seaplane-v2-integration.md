# ROSA V2 Seaplane Platform API Integration in CAPA

- **Status:** Implemented (experimental, feature-gated)
- **Issue:** [ROSAENG-75013](https://redhat.atlassian.net/browse/ROSAENG-75013) — Enable CAPA for V2 Platform API
- **Work item:** [ROSAENG-75904](https://redhat.atlassian.net/browse/ROSAENG-75904)
- **Branch:** `seaplane-v2` in `openshift-online/cluster-api-provider-aws`

## Summary

This proposal describes the integration of the Cluster API Provider AWS
(CAPA) ROSA hosted control plane support with the **v2 Seaplane Platform
API** — the regional, AWS-native backend with AWS IAM and STS web identity
federation — through the `rosa-seaplane-api` clientset SDK. It replicates
the enablement pattern already established in the `rosa` CLI and
`terraform-provider-rhcs` repositories: a SigV4-authenticated clientset,
no OCM login, and a feature-gated reconciliation path that leaves the
existing OCM flow untouched.

The integration covers the sanity scope of the parent epic: **create a
hosted control plane (HCP) cluster, create node pools, and delete the
cluster**, verified by a new e2e suite.

## Motivation

ROSA Classic (v1) clients talk to the OpenShift Cluster Manager (OCM) API,
which requires an OCM token and organization. The v2 Seaplane Platform API
removes that dependency: requests are signed with AWS SigV4 using the
customer's own IAM credentials, and the account is identified by the STS
caller identity. The `rosa` CLI and the Terraform provider have already
added v2 support on top of the `rosa-seaplane-api` clientset; CAPA needs
the same capability so ROSA hosted clusters can be provisioned through
Cluster API against the v2 platform.

## Goals

- Enable `ROSAControlPlane` and `ROSAMachinePool` reconciliation against
  the v2 Platform API via the clientset SDK, gated behind a feature flag
  with the OCM path fully preserved.
- Map the existing CRD specs onto the Platform API `v1alpha1` object model
  (Cluster, NodePool, OidcConfig).
- Provide a sanity e2e test that proves the end-to-end integration
  (cluster + node pools, create and delete), modeled on the rosa CLI and
  Terraform provider sanity tests.

## Non-goals / not yet supported

The following are **not implemented** because the v2 Platform API does not
support them yet (tracked as follow-ups):

- **Kubeconfig / admin credentials** — no Platform API endpoint exists, so
  a CAPI `Cluster` cannot obtain a workload-cluster kubeconfig and the full
  CAPI quick-start flow does not complete. The sanity e2e verifies the
  control plane lifecycle only.
- **Cluster upgrades** (`ControlPlaneUpgradePolicy`) — types exist in the
  API but are not reconciled.
- **Node pool upgrades** — changing a node pool's release image is ignored
  by the drift reconciler.
- **Log forwarders (CloudWatch/S3)** — OCM-only feature, no public API
  equivalent.
- **`ProviderIDList` reconciliation** — EC2 instance discovery for the CAPI
  MachinePool status is not wired up on the seaplane path.
- Component routes / ingress configuration mapping.

## Architecture

### Feature gate

All seaplane code paths are gated behind the `ROSASeaplaneV2` feature gate
(alpha, default **off**) in `feature/feature.go`. It is composed with the
existing `ROSA` gate: `ROSA` registers the ROSA controllers, and
`ROSASeaplaneV2` switches their reconciliation from OCM to the Platform
API. Both gates are wired through the manager deployment manifest:

```yaml
--feature-gates=...,ROSA=${EXP_ROSA:=false},ROSASeaplaneV2=${EXP_ROSA_SEAPLANE_V2:=false}
```

Each controller branches right after finalizer management; the OCM code
path is completely unchanged:

```
Reconcile
 └─ AddFinalizer
     └─ if feature.Gates.Enabled(feature.ROSASeaplaneV2)
         └─ reconcileSeaplaneNormal / reconcileSeaplaneDelete   (new)
     └─ else
         └─ OCM path (unchanged)
```

### Platform API client

`pkg/rosa/seaplaneclient.go` provides two constructors, mirroring the rosa
CLI's `WithSeaplane`:

- **`NewSeaplaneClient(awsCfg, creator, region)`** — used by the
  ROSAControlPlane controller, which already holds an AWS creator
  identity.
- **`NewSeaplaneClientForSession(ctx, awsCfg, region)`** — used by the
  ROSAMachinePool controller; derives the caller identity (account ID,
  ARN, partition) via STS `GetCallerIdentity`.

Both build a `seaplaneclientset.Interface` from:

| Input | Source |
|---|---|
| `Host` | `SEAPLANE_URL` environment variable (HTTPS-validated) |
| `Region` | `RosaControlPlaneSpec.region`, or extracted from the URL; mismatch is an error |
| `AccountID` / `CallerARN` | STS caller identity (SigV4 signed headers) |
| Credentials | The controller's AWS session (honors `identityRef`, incl. cross-account assumption) |

No OCM token or secret is involved. `WithSeaplaneAPIErrorDetails` unwraps
field-level API validation errors into the reconcile error message.

### Spec mapping

Two pure builders map the CAPA CRDs onto the Platform API types (the
seaplane equivalents of `buildOCMClusterSpec` / `nodePoolBuilder`):

**`pkg/rosa/seaplanecluster.go` — `BuildSeaplaneCluster`**

| RosaControlPlaneSpec | Platform API `v1alpha1.Cluster` |
|---|---|
| `rosaClusterName` | `metadata.name` |
| `oidcID` (or `rosaRoleConfigRef` status) | `spec.oidcConfigId` |
| `rolesRef` (or role config status) | `spec.hostedCluster.platform.aws.rolesRef` |
| `releaseImage` (or `SEAPLANE_RELEASE_IMAGE`) | `spec.hostedCluster.release.image` |
| — | `spec.hostedCluster.dns.baseDomain` is **not set**: the Platform API manages DNS allocation and rejects customer-supplied values (`field is platform-managed and cannot be set by customers`) |
| `subnets[0]` (or `rosaNetworkRef`) | `spec.hostedCluster.platform.aws.cloudProviderConfig.subnet.id`; VPC and AZ are resolved via EC2 `DescribeSubnets`, like the rosa CLI |
| `network.machineCIDR/podCIDR/serviceCIDR` | `spec.hostedCluster.networking` (OVNKubernetes, rosa CLI CIDR defaults) |
| `endpointAccess` | `spec.hostedCluster.platform.aws.endpointAccess` |
| `additionalTags`, `deleteProtection` | `spec.tags`, `spec.deleteProtection` |

**`pkg/rosa/seaplanenodepool.go` — `BuildSeaplaneNodePool`**

| RosaMachinePoolSpec | Platform API `v1alpha1.NodePool` |
|---|---|
| `nodePoolName` | `metadata.name` = `<cluster-name>.<nodePoolName>` (the `<cluster>.<child>` convention resolves the parent cluster) |
| `instanceType`, `subnet` | `spec.nodePool.platform.aws.instanceType` / `.subnet.id` |
| — | `spec.nodePool.platform.aws.instanceProfile` = `<operator-roles-prefix>-ROSA-Worker-Role`, derived from the cluster's RolesRef |
| `version` | validated against the cluster's running version; the release image is **only sent when a version is requested** (rosa CLI pattern — otherwise the platform defaults the pool to the cluster's release) |
| MachinePool `replicas` / `autoscaling` | `spec.nodePool.replicas` / `.autoScaling` |
| `labels`, `taints`, `additionalTags`, `tuningConfigs`, `autoRepair`, `nodeDrainGracePeriod`, `volumeSize`, `imageType`, `spotMarketOptions` | corresponding NodePool fields |

### Reconciliation flow

**ROSAControlPlane** (`controlplane/rosa/controllers/rosacontrolplane_controller_seaplane.go`):

1. Get the cluster by name from the Platform API.
2. If missing: resolve inputs (role config, ROSANetwork, subnet → VPC/AZ
   via EC2) and create it; record `status.id` (the API UID).
3. If present: reconcile **mutable** fields (delete protection, tags) via
   `Clusters().Update` once the phase is `Ready`; map the phase onto
   `ROSAControlPlaneReadyCondition`, `status.ready`, `status.version` and
   `spec.controlPlaneEndpoint`.
4. On delete: honor delete protection, call `Clusters().Delete`, wait for
   the cluster to disappear, then remove the finalizer.

**ROSAMachinePool** (`exp/controllers/rosamachinepool_controller_seaplane.go`):

1. Get the node pool by `<cluster>.<np>` name.
2. If missing: fetch the parent cluster (release image, worker instance
   profile, default subnet) and create the node pool.
3. If present: reconcile mutable fields (replicas, autoscaling, autoRepair,
   labels, tags, drain timeout — **not** the release image) and map the
   phase onto `RosaMachinePoolReadyCondition` / `status.ready` /
   `status.replicas`; sync external-autoscaler replica counts back to the
   CAPI MachinePool.
4. On delete: honor delete protection, call `NodePools().Delete`, remove
   the finalizer.

Both controllers expose an injectable `NewSeaplaneClient` factory (mirroring
`NewOCMClient`) so unit tests can substitute the clientset's generated fake.

### API changes

`RosaControlPlaneSpec` (v1beta2) gained two immutable, optional fields —
`controlplane/rosa` is v1beta2-only, so no v1beta1 conversions are needed:

- **`releaseImage`** — the OCP release payload image; required on the
  seaplane path (the OCM path ignores it and derives the payload from
  `version`). Falls back to the `SEAPLANE_RELEASE_IMAGE` env var.
- **`baseDomain`** — removed: the Platform API rejects customer-supplied
  DNS base domains (`field is platform-managed`); DNS is auto-allocated.

### Dependencies

`go.mod` requires `rosa-seaplane-api/api` and `rosa-seaplane-api/clientset`
via **local `replace` directives** pointing at `../rosa-seaplane-api` (the
published clientset lags the local tree). This transitively adds
`github.com/openshift/hypershift/api` (the passthrough types embed
HyperShift types) and forces the module's `go` directive to 1.26.5.
Before merging upstream, the replaces should be swapped for published
module versions.

### Manager configuration

`config/manager/manager.yaml` passes the feature gates into the controller
deployment; both are overridable at deploy time via clusterctl variables
(sourced from OS environment variables):

| Variable | Purpose |
|---|---|
| `EXP_ROSA` | enable the ROSA controllers |
| `EXP_ROSA_SEAPLANE_V2` | switch reconciliation to the Platform API |

The `SEAPLANE_*` runtime variables (`SEAPLANE_URL`,
`SEAPLANE_RELEASE_IMAGE`, `SEAPLANE_BASE_DOMAIN`) are **not** declared in
the provider components: clusterctl treats a variable with an empty default
— like the optional `SEAPLANE_BASE_DOMAIN` — as missing and fails variable
substitution for every user of the components. Instead, the seaplane e2e
suite patches them into the `capa-controller-manager` deployment after
`clusterctl init` (see `rosa_seaplane_manager_env.go`); the env change
triggers a rollout before the first spec runs. The controller reads them
the same way in any other deployment — set them as pod environment
variables however your platform allows.

## Testing

### Unit tests

- `pkg/rosa/seaplaneclient_test.go` — URL/region validation, client
  construction, region conflict.
- `pkg/rosa/seaplanecluster_test.go` — cluster mapping, defaults, errors.
- `pkg/rosa/seaplanenodepool_test.go` — node pool mapping, drift rules.
- `controlplane/rosa/controllers/rosacontrolplane_controller_seaplane_test.go`
  and `exp/controllers/rosamachinepool_controller_seaplane_test.go` —
  controller create/ready/provisioning/update/delete flows using the
  clientset's generated fake and a fake k8s client with a static
  controller identity.

Run everything with the usual targets (envtest binaries are fetched
automatically by `make test`):

```bash
make generate && make lint && make test
```

### e2e sanity test

`test/e2e/suites/rosa_seaplane/rosa_seaplane_sanity_test.go` is modeled on
the terraform-provider-rhcs seaplane sanity test. It:

1. Creates a **managed OIDC config** via the Platform API and waits for
   `Ready`.
2. Creates the **8 operator/worker IAM roles** via AWS IAM (mirroring
   `rosa create operator-roles --seaplane`), with OIDC-federated trust
   policies and ROSA managed policies.
3. Applies a CAPI `Cluster` + `ROSAControlPlane` to the management
   cluster and waits for `status.ready` (90 min timeout).
4. Applies two `MachinePool` + `ROSAMachinePool` pairs (`np1` ×2 replicas,
   `np2` ×1) and waits for both to become `Ready` (45 min timeout).
5. Cross-checks the cluster and node pools directly through the Platform
   API.
6. Deletes the machine pools and the control plane, verifying everything
   is removed (cluster deletion, 60 min timeout).
7. Tears down the IAM roles and OIDC config (LIFO).

#### Prerequisites

- **AWS credentials** with permission to create IAM roles, OIDC-related
  resources, and to call the Platform API (SigV4). Note that the e2e
  framework normally replaces `AWS_B64ENCODED_CREDENTIALS` with the
  CloudFormation bootstrapper user's keys, which the Platform API
  rejects — the seaplane suite captures your value at startup and
  restores it into the manager's credentials secret, so encode from a
  principal the Platform API accepts:
  ```bash
  export AWS_B64ENCODED_CREDENTIALS=$(go run ./cmd/clusterawsadm bootstrap credentials encode-as-profile \
    --profile <profile> --region us-east-1)
  ```
  ⚠️ If the profile assumes a role, the encoded credentials are
  **temporary** (default 1h). Encode right before the run; a run that
  outlives the session fails AWS calls mid-flight.
- **Docker** (the suite runs a kind management cluster) and the standard
  capa e2e tooling (`make e2e-image` builds the controller image).
- **A pre-existing VPC subnet** for the hosted control plane and worker
  nodes — the test does not create a VPC.

#### Environment variables

| Variable | Required | Description |
|---|---|---|
| `SEAPLANE_URL` | yes | Platform API base URL; the AWS region is derived from it |
| `SEAPLANE_RELEASE_IMAGE` | yes | e.g. `quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64` |
| `SEAPLANE_SUBNET_ID` | yes | worker/control plane subnet ID |
| `SEAPLANE_CLUSTER_NAME` | no | defaults to `sp-e2e-<timestamp>` |
| `SEAPLANE_OPERATOR_ROLES_PREFIX` | no | defaults to the cluster name |
| `SEAPLANE_BASE_DOMAIN` | no | DNS base domain; empty ⇒ auto-allocated |
| `SEAPLANE_NODEPOOL_INSTANCE_TYPE` | no | defaults to `m5.xlarge` |
| `AWS_REGION` | recommended | must match the region in `SEAPLANE_URL` |

#### Running the test

From the repository root:

```bash
export SEAPLANE_URL="https://<api-id>.execute-api.us-east-1.amazonaws.com"
export SEAPLANE_RELEASE_IMAGE="quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64"
export SEAPLANE_SUBNET_ID="subnet-0123456789abcdef0"
export AWS_REGION="us-east-1"

make test-e2e-rosa-seaplane
```

The target deploys capa into a kind management cluster with
`EXP_ROSA=true EXP_ROSA_SEAPLANE_V2=true` and the `SEAPLANE_*` variables
passed through to the controller manager, then runs the sanity spec.
Expect roughly 45–90 minutes end to end (cluster provisioning dominates).

To focus/filter or keep the kind cluster for inspection, use the standard
ginkgo knobs, e.g.:

```bash
GINKGO_ARGS="-stream -timeout=3h" make test-e2e-rosa-seaplane
```

#### Verifying manually with the rosa CLI

The rosa CLI's seaplane commands can be used to cross-check what the test
created (they talk to the same Platform API):

```bash
rosa list clusters --seaplane-url "$SEAPLANE_URL"
rosa list machinepools -c <cluster-name> --seaplane-url "$SEAPLANE_URL"
rosa describe cluster -c <cluster-name> --seaplane-url "$SEAPLANE_URL"
```

### What "success" means today

Because kubeconfig retrieval is not yet supported by the Platform API, the
sanity test validates the **control plane lifecycle** only: the cluster
and node pools exist on the Platform API, reach `Ready`, and are cleanly
deleted. Applying full workload manifests through CAPI (machines,
deployments) requires the kubeconfig follow-up.

## Implementation history

| Commit | Content |
|---|---|
| `51f713e10` | Clientset integration, `ROSASeaplaneV2` gate, cluster create/get/delete, manager wiring, sanity e2e suite |
| `060362292` | `releaseImage`/`baseDomain` CR fields, mutable-field drift reconciliation for clusters |
| `3b5a20d52` | ROSAMachinePool → NodePool reconciliation (create/update/delete), node pools in the sanity e2e |

## Future work

1. Swap local `replace` directives for published `rosa-seaplane-api`
   module versions.
2. Kubeconfig/admin credentials once the Platform API exposes them —
   unlocks the full CAPI quick-start flow.
3. `ProviderIDList` reconciliation (EC2 instance discovery by node pool
   tags).
4. Cluster and node pool upgrades (`ControlPlaneUpgradePolicy`, release
   image changes).
5. Component routes / ingress configuration mapping.
6. Consider promoting `ROSASeaplaneV2` beyond alpha once the sanity e2e
   runs reliably in CI.
