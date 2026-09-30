# Argo CD with Ambit (GitOps mode)

In GitOps mode the console opens pull requests in your configuration repository; Argo CD applies what
is merged. Three pieces:

1. **Health check** so Argo CD shows Ambit objects as Healthy, Progressing or Degraded — add
   `health.lua` to `argocd-cm` once per kind: `resource.customizations.health.ambitproject.dev_Model`,
   `..._Server` and `..._AccessRule` (ConfigMap keys cannot hold a `*`). With the argo-cd Helm chart:

   ```sh
   helm upgrade --install argocd argo-cd --repo https://argoproj.github.io/argo-helm -n argocd \
     --set-file 'configs.cm.resource\.customizations\.health\.ambitproject\.dev_Model=deploy/argocd/health.lua' \
     --set-file 'configs.cm.resource\.customizations\.health\.ambitproject\.dev_Server=deploy/argocd/health.lua' \
     --set-file 'configs.cm.resource\.customizations\.health\.ambitproject\.dev_AccessRule=deploy/argocd/health.lua'
   ```

2. **An Application** for the directory of Ambit objects — see `application.yaml`. Layout in the
   repository: `<path>/accessrules/<name>.yaml`, `<path>/servers/<name>.yaml`, `<path>/models/<name>.yaml`.
   The console writes and edits files at exactly these paths.

3. **The chart values** `gitops.enabled: true`, `gitops.repository`, `gitops.github.appID`, and a Secret
   `ambit-github-app` with key `private-key.pem` (the GitHub App's key, Contents and Pull requests: write,
   installed only on this repository).

Why Argo CD and Ambit do not fight: the controller never copies Argo CD's tracking annotation onto the
objects it generates, and writes only status on Ambit objects; Argo CD ignores status. Generated objects
carry an owner reference, so Argo CD shows them as children and never prunes them.
