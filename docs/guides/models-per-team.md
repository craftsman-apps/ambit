---
title: Different models for different teams
description: Allow the engineering group one model and the sales group another, and see the allowed call succeed and the other one refused.
---

# Different models for different teams

**What you get:** each team uses only the models you give it. Alice in engineering can call the large model. Bob in sales can call the small one. Each is refused on the other.

## Before you start

You need the cluster from the [Quick start](../quick-start.md), with its model `free-model` and the OpenRouter Secret `ambit-llm-openrouter`. Start it with an OpenRouter key (`ambit local up --openrouter-key-file <file>`): the test model that `ambit local up` uses without a key is not `free-model`.

Access by group needs sign-in through an identity provider that sends a `groups` claim. The quick-start cluster has only the bootstrap admin, who has no groups. Two ways to go on:

- **Your identity provider.** Add its Dex connector as described in [Install on your cluster](../install.md). Create the groups `engineering` and `sales`. Put Alice in the first and Bob in the second.
- **A test directory.** If you have no identity provider yet, use the small directory below. It is for trying the guides only. It holds Alice and Dan in `engineering` and Bob in `sales`, and it connects to Ambit as an LDAP directory.

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

    dn: cn=alice,ou=people,dc=acme,dc=test
    objectClass: inetOrgPerson
    cn: alice
    sn: Alice
    mail: alice@acme.test
    userPassword: alice-pass

    dn: cn=bob,ou=people,dc=acme,dc=test
    objectClass: inetOrgPerson
    cn: bob
    sn: Bob
    mail: bob@acme.test
    userPassword: bob-pass

    dn: cn=dan,ou=people,dc=acme,dc=test
    objectClass: inetOrgPerson
    cn: dan
    sn: Dan
    mail: dan@acme.test
    userPassword: dan-pass

    dn: cn=engineering,ou=groups,dc=acme,dc=test
    objectClass: groupOfNames
    cn: engineering
    member: cn=alice,ou=people,dc=acme,dc=test
    member: cn=dan,ou=people,dc=acme,dc=test

    dn: cn=sales,ou=groups,dc=acme,dc=test
    objectClass: groupOfNames
    cn: sales
    member: cn=bob,ou=people,dc=acme,dc=test
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
            - { name: LDAP_DOMAIN, value: acme.test }
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

Open <http://console.localtest.me:8080>. The sign-in page now offers **Acme directory**. Sign in as `alice@acme.test` with the password `alice-pass`. The console home page shows the group `engineering`. The other accounts are `bob@acme.test` with `bob-pass` and `dan@acme.test` with `dan-pass`.

</details>

Both ways need one more step. Each person signs in to the console and creates a personal API key, as described in [Connect Claude Code, Codex, opencode, Cursor and SDKs](coding-tools.md). A key carries the groups its owner had at their last console sign-in. Save Alice's key in the shell variable `ALICE_KEY` and Bob's key in `BOB_KEY`.

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

## Check it works

The gateway needs a few seconds to load new rules. Define this function, then make the four calls. It prints the answer and the HTTP status.

```sh
ask() { # ask KEY MODEL
  curl -s -w ' HTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
    -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in five words.\"}]}"
}
ask "$ALICE_KEY" nvidia/nemotron-3-super-120b-a12b:free
ask "$ALICE_KEY" nvidia/nemotron-3.5-lightning:free
ask "$BOB_KEY" nvidia/nemotron-3.5-lightning:free
ask "$BOB_KEY" nvidia/nemotron-3-super-120b-a12b:free
```

Alice's first call and Bob's first call end with `HTTP 200` and an answer. The other two end with:

```text
{"error":{"message":"Model authorization denied","type":"invalid_request_error","code":"model_authorization_denied"}} HTTP 403
```

Each person can also see their own access. The console home page lists the models and servers that the rules give them.

## Related

- [Concepts](../concepts.md): how Ambit checks a request.
- [Models and providers](../providers.md): other providers.
- [Spending limits](spending-limits.md): limit what each team may spend.
