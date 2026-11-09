---
title: Different models for different teams
description: Allow the engineering group one model and the sales group another, and see the allowed call succeed and the other one refused.
---

# Different models for different teams

**What you get:** each team uses only the models you give it. User01 in engineering can call the large model. User02 in sales can call the small one. Each is refused on the other.

## Before you start

You need the cluster from the [Quick start](../quick-start.md), with its model `free-model` and the OpenRouter Secret `ambit-llm-openrouter`. Start it with an OpenRouter key (`ambit local up --openrouter-key-file <file>`): the test model that `ambit local up` uses without a key is not `free-model`.

Access by group needs sign-in through an identity provider that sends a `groups` claim. The quick-start cluster has only the bootstrap admin, who has no groups. Two ways to go on:

- **Your identity provider.** Add its Dex connector as described in [Install on your cluster](../install.md). Create the groups `engineering` and `sales`. Put User01 in the first and User02 in the second.
- **A test directory.** If you have no identity provider yet, use the small directory below. It is for trying the guides only. It holds User01 and User04 in `engineering` and User02 in `sales`, and it connects to Ambit as an LDAP directory.

<details>
<summary>The test directory</summary>

Save this as `acme-ldap.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata: { name: acme-ldif }
data:
  people.ldif: |
    dn: ou=people,dc=acme,dc=test
    objectClass: organizationalUnit
    ou: people

    dn: ou=groups,dc=acme,dc=test
    objectClass: organizationalUnit
    ou: groups

    dn: cn=user01,ou=people,dc=acme,dc=test
    objectClass: inetOrgPerson
    cn: user01
    sn: User01
    mail: user01@example.com
    userPassword: user01-pass

    dn: cn=user02,ou=people,dc=acme,dc=test
    objectClass: inetOrgPerson
    cn: user02
    sn: User02
    mail: user02@example.com
    userPassword: user02-pass

    dn: cn=user04,ou=people,dc=acme,dc=test
    objectClass: inetOrgPerson
    cn: user04
    sn: User04
    mail: user04@example.com
    userPassword: user04-pass

    dn: cn=engineering,ou=groups,dc=acme,dc=test
    objectClass: groupOfNames
    cn: engineering
    member: cn=user01,ou=people,dc=acme,dc=test
    member: cn=user04,ou=people,dc=acme,dc=test

    dn: cn=sales,ou=groups,dc=acme,dc=test
    objectClass: groupOfNames
    cn: sales
    member: cn=user02,ou=people,dc=acme,dc=test
---
apiVersion: v1
kind: Secret
metadata: { name: acme-ldap }
stringData: { password: ldap-admin-pass }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: acme-ldap }
spec:
  replicas: 1
  selector: { matchLabels: { app: acme-ldap } }
  template:
    metadata: { labels: { app: acme-ldap } }
    spec:
      containers:
        - name: ldap
          image: osixia/openldap:1.5.0
          args: ["--copy-service"]
          env:
            - { name: LDAP_ORGANISATION, value: Acme }
            - { name: LDAP_DOMAIN, value: example.com }
            - { name: LDAP_ADMIN_PASSWORD, value: ldap-admin-pass }
            - { name: LDAP_TLS, value: "false" }
          ports: [{ containerPort: 389 }]
          volumeMounts:
            - { name: ldif, mountPath: /container/service/slapd/assets/config/bootstrap/ldif/custom/people.ldif, subPath: people.ldif }
      volumes:
        - { name: ldif, configMap: { name: acme-ldif } }
---
apiVersion: v1
kind: Service
metadata: { name: acme-ldap }
spec:
  selector: { app: acme-ldap }
  ports: [{ port: 389 }]
```

Save this as `sso-values.yaml`:

```yaml
sso:
  connectors:
    - type: ldap
      id: acme
      name: Acme directory
      config:
        host: acme-ldap.ambit-system.svc:389
        insecureNoSSL: true
        bindDN: cn=admin,dc=acme,dc=test
        bindPW: $LDAP_BIND_PW
        usernamePrompt: Email
        userSearch:
          baseDN: ou=people,dc=acme,dc=test
          filter: "(objectClass=inetOrgPerson)"
          username: mail
          idAttr: DN
          emailAttr: mail
          nameAttr: cn
        groupSearch:
          baseDN: ou=groups,dc=acme,dc=test
          filter: "(objectClass=groupOfNames)"
          userMatchers: [{ userAttr: DN, groupAttr: member }]
          nameAttr: cn
dex:
  envVars:
    - name: LDAP_BIND_PW
      valueFrom: { secretKeyRef: { name: acme-ldap, key: password } }
```

Start the directory and connect it. The same upgrade also turns on budgets, which the [Spending limits](spending-limits.md) guide needs:

```sh
kubectl -n ambit-system apply -f acme-ldap.yaml
kubectl -n ambit-system rollout status deploy/acme-ldap --timeout=180s
helm upgrade ambit oci://ghcr.io/craftsman-apps/charts/ambit --version 0.1.0 -n ambit-system \
  --reuse-values -f sso-values.yaml --set limits.enabled=true --wait --timeout 10m
```

Open <http://console.localtest.me:8080>. The sign-in page now offers **Acme directory**. Sign in as `user01@example.com` with the password `user01-pass`. The console home page shows the group `engineering`. The other accounts are `user02@example.com` with `user02-pass` and `user04@example.com` with `user04-pass`.

</details>

Both ways need one more step. Each person signs in to the console and creates a personal API key, as described in [Connect your coding agents](coding-tools.md). A key carries the groups its owner had at their last console sign-in. Save User01's key in the shell variable `USER01_KEY` and User02's key in `USER02_KEY`.

## 1. Add a second model

The quick start gave you `free-model`. Add a second model that uses the same Secret:

```sh
kubectl -n ambit-system apply -f - <<'EOF'
apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: sales-model }
spec:
  displayName: Sales model
  provider: Openrouter
  model: nvidia/nemotron-3.5-lightning:free
  credentialsRef: { name: ambit-llm-openrouter }
EOF
kubectl -n ambit-system wait models.ambitproject.dev/sales-model --for=condition=Ready --timeout=120s
```

OpenRouter changes its free models often. If this one is gone, use another model whose name ends in `:free`.

## 2. Replace the open rule with two group rules

An `AccessRule` only adds access. The rule `everyone-local` from `ambit local up` (`everyone-free-model` if you used the [by-hand steps](../local-up.md)) lets everyone use `free-model`, so delete it first. Then create one rule for each group:

```sh
kubectl -n ambit-system delete accessrule everyone-local everyone-free-model --ignore-not-found
kubectl -n ambit-system apply -f - <<'EOF'
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: engineering-models }
spec:
  groups: [engineering]
  models: [free-model]
---
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: sales-models }
spec:
  groups: [sales]
  models: [sales-model]
EOF
```

The group names are the names your identity provider sends. A person in any listed group is allowed.

To let in one person who is not in the group, such as a pilot user or a contractor, add `users` with their email address next to `groups` (for example `users: [dana@example.com]`). Prefer groups; use people for exceptions. People are matched by a verified email. If your identity provider does not send `email_verified`, set `accessRules.usersRequireVerifiedEmail: false` only if users cannot change their own email.

## Check it works

The gateway needs a few seconds to load new rules. Define this function, then make the four calls. It prints the answer and the HTTP status.

```sh
ask() { # ask KEY MODEL
  curl -s -w ' HTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
    -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in five words.\"}]}"
}
ask "$USER01_KEY" nvidia/nemotron-3-super-120b-a12b:free
ask "$USER01_KEY" nvidia/nemotron-3.5-lightning:free
ask "$USER02_KEY" nvidia/nemotron-3.5-lightning:free
ask "$USER02_KEY" nvidia/nemotron-3-super-120b-a12b:free
```

User01's first call and User02's first call end with `HTTP 200` and an answer. The other two end with:

```text
{"error":{"message":"Model authorization denied","type":"invalid_request_error","code":"model_authorization_denied"}} HTTP 403
```

Each person can also see their own access. The console home page lists the models and servers that the rules give them.

## Related

- [Concepts](../concepts.md): how Ambit checks a request.
- [Models and providers](../providers.md): other providers.
- [Spending limits](spending-limits.md): limit what each team may spend.
