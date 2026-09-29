# Third-party CRDs

Gateway API (standard channel) and agentgateway CRDs, vendored at pinned versions by `make vendor-crds`.

They are applied with server-side apply, not through Helm:

```sh
kubectl apply --server-side --force-conflicts -f deploy/crds/
```

Why not Helm: together they exceed Helm's 1 MiB release limit, and a Helm-owned CRD is deleted on
`helm uninstall`, which would delete every object of that type. Applied this way, uninstalling Ambit never removes them.

Argo CD: point one Application at this directory with `syncOptions: [ServerSideApply=true]` and sync-wave "-2".
