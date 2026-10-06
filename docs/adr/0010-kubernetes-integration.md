# ADR-0010: Kubernetes integration and NetworkPolicy

- Status: Accepted (client choice revised on 2026-10-06, D-055)
- Date: 2026-10-06
- Phases: 17 (read-only, CNI detection), 18 (NetworkPolicy plan, drift), 20 (enforcement)

## Context

Sentinel should observe a cluster (version, namespaces, pods, services,
nodes), detect the CNI and whether NetworkPolicy is likely enforced,
generate NetworkPolicy plans, detect drift and — only with explicit
enforcement — apply manifests. It is **not** a Kubernetes controller.

The Kubernetes provider is separate from the host firewall: it has no
backend in common with nftables/iptables. Shared pieces are limited to
`internal/netspec` (CIDR/port/protocol validation), the event model, the
mode/safety gates (ADR-0003) and the audit log.

## Options considered (client)

| Option | Pros | Cons |
|---|---|---|
| **`k8s.io/client-go`** (Apache-2.0, official) | Complete auth (exec plugins, OIDC via plugins), informers, fake clientset | Large dependency graph and binary growth; release cadence tied to Kubernetes minors; most of it unused for read-only discovery |
| Minimal stdlib REST client | No dependencies; GET-only gate is a `RoundTripper`; tests with a fake API server (`httptest`) | We implement kubeconfig parsing (YAML lib already present), token/cert/CA auth and the `ExecCredential` protocol |
| Separate `sentinel-k8s` binary built with client-go | Keeps the core binary small | Second binary to package and supervise |

## Decision

Kubernetes is a committed goal (the maintainer runs Linux-only,
container and Kubernetes environments), so auth coverage and long-term
maintenance outweigh dependency size:

- **`k8s.io/client-go`** (+ `k8s.io/api`, `k8s.io/apimachinery`), confined
  to `internal/kubernetes/client`; no other package imports Kubernetes
  modules. It covers in-cluster config, kubeconfig with contexts, client
  certificates, tokens and exec credential plugins (EKS/GKE/AKS,
  kubelogin) exactly like `kubectl`, and provides the fake clientset the
  specification asks for in tests.
- The read-only gate is a `rest.Config.WrapTransport` round-tripper with
  the verb allowlist of ADR-0004, plus a constructor that never builds a
  mutating client in `read_only`.
- **Cost control**: versions pinned and updated by Dependabot one
  Kubernetes minor at a time; binary size and module count measured at
  adoption (Phase 17) and recorded in the decision log; a `nokubernetes`
  build tag can produce a slim binary without the Kubernetes provider if
  size becomes a problem.
- Superseded option: the stdlib REST client (re-implementing kubeconfig
  and exec-credential handling) — less code to depend on, but more
  security-sensitive code to own.
- **Read-only verification**: `SelfSubjectRulesReview` /
  `SelfSubjectAccessReview` report Sentinel's effective permissions. If
  the credentials can mutate resources while `mode: read_only`, status
  shows `over_privileged` and an event recommends a read-only RBAC role
  (shipped as an example manifest).
- **Discovery** (`internal/kubernetes/discovery`): `/version`, namespaces
  (filtered by `namespaces`), pods, services, nodes, NetworkPolicies;
  periodic list every `discovery_interval` with watch where cheap.
- **CNI detection** with a confidence level: kube-system DaemonSets and
  images, known CRDs (e.g. Calico, Cilium, Antrea), node annotations.
  Output `cni: {name, version?, confidence}` and
  `network_policy_enforcement: likely | unlikely | unknown`. **Never
  `supported`** from heuristics alone: creating a NetworkPolicy object
  does not prove it is enforced (no-op without a policy-capable CNI).
  Active verification (test pods) is invasive and belongs to the
  enforcement phase as an explicit opt-in.
- **NetworkPolicy planner** (`internal/kubernetes/networkpolicy`,
  `planner`, Phase 18): templates (default-deny ingress/egress per namespace,
  allow DNS egress, allow same-namespace, allow from labelled namespaces),
  generation of `networking.k8s.io/v1` manifests labelled
  `app.kubernetes.io/managed-by: sentinel`, validation, diff against live
  objects, drift detection for managed objects only, and server-side
  `dryRun=All` in effective mode `dry_run`.
- **Enforcement (Phase 20)**: server-side apply with field manager `sentinel`,
  only for managed objects; never deletes or edits unmanaged policies;
  audit + events + post-apply read-back. Admission integration and
  CNI-specific (eBPF) policies are out of scope.

### Configuration harmonisation (accepted, D-048)

The specification's `network_policy.enforcement: false` duplicates the
domain-wide `mode`. Decided:

```yaml
kubernetes:
  enabled: false
  mode: read_only            # read_only | enforce (enforce only from Phase 20)
  kubeconfig: ""             # empty = $KUBECONFIG if set, else ~/.kube/config; never required
  context: ""
  in_cluster: false
  namespaces: []
  discovery_interval: 60s
  network_policy:
    enabled: false
    dry_run: true
    managed_only: true
```

`kubeconfig: ${KUBECONFIG}` from the specification would make the whole
configuration fail to load when the variable is undefined (D-006), even
with `enabled: false`; the empty default avoids that.

## Consequences

- client-go is the second runtime dependency family; recorded with
  version, license and size impact when Phase 17 adopts it.
- Status and docs must always state that NetworkPolicy effect depends on
  the CNI.
- Unit tests use the client-go fake clientset; a `kind`/k3s-based
  integration job runs separately (GitHub-hosted runner or the
  maintainer's VMs).
