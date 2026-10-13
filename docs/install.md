---
title: Install on your cluster
description: Install Ambit on a Kubernetes cluster with Helm, connect your identity provider, set up TLS, and check the production settings.
---

# Install on your cluster

This page installs Ambit on a real cluster. To try Ambit on a laptop first, use the [Quick start](quick-start.md).

Ambit is installed with two Helm charts. The charts and the console image are published at `ghcr.io/craftsman-apps`. Ambit is Alpha software: the API is `ambitproject.dev/v1alpha1` and can change between releases.

## Requirements

- Kubernetes 1.30 or later. The chart refuses to install on an older cluster.
- Helm 3.
- A public HTTPS address for the console, and one for the LLM gateway (and one for MCP, if you use it). Cursor, for example, sends its requests from its own servers, so the address must be reachable from the internet.
- An identity provider that Dex can connect to (OIDC, SAML, LDAP, GitHub and others), or one that issues OIDC tokens.

## Install

Install the dependency CRDs, the Ambit CRDs, and then Ambit. Replace `0.1.0` with the release you want.

```sh
R=https://github.com/craftsman-apps/ambit/releases/download/v0.1.0
curl -fsSLO $R/crds.yaml
kubectl apply --server-side -f crds.yaml
helm install ambit-crds oci://ghcr.io/craftsman-apps/charts/ambit-crds --version 0.1.0 -n ambit-system --create-namespace --wait
```

Write a values file, `values.yaml`. This one is the smallest useful production file: HTTPS addresses, the LLM and MCP gateways, and the audit log.

```yaml
console:
  url: https://console.example.com
  host: console.example.com
gateway:
  replicas: 2
llm:
  enabled: true
  host: llm.example.com
mcp:
  enabled: true
  host: mcp.example.com
evidence:
  enabled: true
limits:
  enabled: true
protection:
  enabled: true
```

```sh
helm install ambit oci://ghcr.io/craftsman-apps/charts/ambit --version 0.1.0 -n ambit-system -f values.yaml --wait --timeout 10m
```

`console.host` must be the host of `console.url`. Point DNS for the three hosts at the gateway's Service (`kubectl -n ambit-system get service`; with `gateway.service.type: LoadBalancer` it has an external address).

The chart `values.yaml` lists every setting with a comment. The sections below cover the ones most installs change.

## Sign-in for your company

The console signs people in through Dex, which Ambit installs. Add the connector of your identity provider under `sso.connectors`. The format is Dex's: see the [Dex connector documentation](https://dexidp.io/docs/connectors/). Never put a secret in the values. Write `$ENV_NAME` and supply the variable from a Secret with `dex.envVars`.

```yaml
sso:
  connectors:
    - type: oidc
      id: company
      name: Company sign-in
      config:
        issuer: https://login.example.com
        clientID: ambit
        clientSecret: $COMPANY_CLIENT_SECRET
        redirectURI: https://console.example.com/dex/callback
        insecureEnableGroups: true
dex:
  envVars:
    - name: COMPANY_CLIENT_SECRET
      valueFrom:
        secretKeyRef: { name: company-sso, key: clientSecret }
```

The login page shows "Continue with Company sign-in". When it works, set `bootstrapAdmin.enabled: false`. The console then deletes the bootstrap account and its password.

### Groups and console roles

Ambit reads the `groups` claim of the token. The claim decides two things:

- **Access rules.** An `AccessRule` lists group names in `spec.groups`.
- **Console roles.** A person in the group `ambit-admins` is an admin. A person in `ambit-approvers` is an approver. Everyone else can only view. Create these two groups in your identity provider and make sure the connector sends groups.

Group changes reach the console at the next sign-in. `console.sessionHours` (default 1) is the longest a session lasts, so keep it short.

### Offboarding

When your identity provider deactivates a person, it tells the console by SCIM. Ambit then revokes the person's keys, sessions and app grants. See [Offboarding](offboarding.md) and [Personal API keys](api-keys.md).

## Tokens for the gateways

The gateways (`/v1` and `/mcp`) accept tokens from one issuer. Choose one.

**Your identity provider's tokens.** Set the issuer for each gateway:

```yaml
llm:
  jwt:
    issuer: https://login.example.com
    audiences: [ambit]
    jwksURL: https://login.example.com/keys
    requireEmail: true
mcp:
  jwt:
    issuer: https://login.example.com
    audiences: [ambit]
    jwksURL: https://login.example.com/keys
    requireEmail: true
```

`requireEmail: true` (the default) refuses tokens that have no usable `email` claim. Offboarding, per-person budgets and audit use the email.

**Ambit's own Dex.** Set `jwt: { dex: true }` under `llm` and `mcp`. The quick start does this. Local accounts then get tokens with the password grant of the public client `ambit-cli`. Use it for tests, not for a company.

Personal API keys work with both choices. They are on by default (`apiKeys.enabled`). `apiKeys.inactivityDays` (default 30) stops a key when its owner has not signed in to the console for that many days, and `apiKeys.maxDays` (30, 90 or 365) is the longest end date a person may choose.

## Models and cloud identity

Create a `Model` for each model, with its credentials in a Secret. For Amazon Bedrock and Google Vertex AI you can leave the credentials out and use the gateway's own cloud identity (IRSA or EKS Pod Identity for AWS, workload identity for Google). Set the role on the gateway's ServiceAccount with `gateway.serviceAccount.annotations`. See [Models and providers](providers.md).

## Your own ClickHouse and Valkey

The chart installs ClickHouse (the audit log) and Valkey (budget counters) in your cluster. Use your own instead for a larger or managed setup.

```yaml
evidence:
  enabled: true
  retention: 8760h            # one year
  clickhouse:
    external:
      host: clickhouse.example.com
      tls: true
      database: otel
      user: ambit
      passwordSecret: ambit-clickhouse-password   # a Secret with the key "password"
limits:
  enabled: true
  valkey:
    external:
      address: valkey.example.com:6379
      tls: true
      passwordSecret: ambit-valkey-password       # a Secret with the key "password"
```

The ClickHouse user needs `CREATE TABLE` in the database once, then `INSERT` and `SELECT`. The collector writes over the native protocol (`nativePort`, default 9440) and the console reads over HTTPS (`httpPort`, default 8443). Without TLS the ports are 9000 and 8123.

Valkey holds the budget counters. The Valkey that the chart installs has no persistence: a restart sets today's counters to zero. An external Valkey with persistence keeps them.

## Argo CD and GitOps

In GitOps mode the console opens pull requests in your configuration repository instead of changing the cluster. Argo CD applies what is merged.

```yaml
gitops:
  enabled: true
  repository: acme/platform-config
  branch: main
  path: ambit
  github:
    appID: "123456"
    privateKeySecret: ambit-github-app   # a Secret with the key private-key.pem
  argocd:
    application: argocd/ambit-config
```

The directory layout, the Argo CD health check and an example Application are in [`deploy/argocd`](https://github.com/craftsman-apps/ambit/tree/main/deploy/argocd) in the repository.

## HTTPS

Create a TLS Secret of type `kubernetes.io/tls` that covers `console.host`, `llm.host` and, if MCP is on, `mcp.host` (for example with cert-manager), and name it in the values:

```yaml
gateway:
  tls:
    enabled: true
    certificateSecret: ambit-tls
```

The gateway then serves HTTPS on `gateway.tls.port` (default 8443), and plain HTTP on the console and LLM hosts redirects to it. Clients use that port in the address unless it is 443. Set `console.url` to the address people open, with `https://`: it is also the issuer address of Dex.

## Upgrade and uninstall

Read [Upgrading](upgrading.md) before each upgrade. Upgrade the CRD chart first, then Ambit:

```sh
helm upgrade ambit-crds oci://ghcr.io/craftsman-apps/charts/ambit-crds --version <new version> -n ambit-system
helm upgrade ambit oci://ghcr.io/craftsman-apps/charts/ambit --version <new version> -n ambit-system -f values.yaml --wait
```

To remove Ambit, run `helm uninstall ambit -n ambit-system`, then `helm uninstall ambit-crds -n ambit-system`. The CRDs carry `helm.sh/resource-policy: keep`, so Helm leaves them, and your objects, in the cluster.

## Production checklist

- `gateway.replicas` is 2 or more. With two replicas the chart also adds a PodDisruptionBudget. A rolling update starts one extra pod first, so the nodes need room for one more.
- `bootstrapAdmin.enabled` is `false` after company sign-in works.
- `console.url` and all hosts use HTTPS, and `gateway.tls` is set.
- `evidence.enabled` is `true`, with a `retention` that fits your rules. Check that audit records reach ClickHouse.
- `limits.enabled` is `true`, and you decided `limits.failureMode`: `FailOpen` (the default) lets requests pass uncounted when the budget store is down; `FailClosed` refuses them.
- Budget counters survive a restart only with an external Valkey that has persistence. The Valkey in the chart has none.
- `protection.enabled` is `true` if you use MCP servers, for tool pinning and masking of personal data.
- Turn on `alerts.enabled` if you run the Prometheus Operator. See [Alerts](alerts.md).
- The console runs as one replica (`console.replicas` must be 1). It is briefly unavailable during an upgrade. LLM and MCP requests keep working.
- Provider credentials are in Secrets. `Model` objects hold only the Secret name, so they are safe in Git.
