---
title: Set spending limits
description: Limit what a group and each person in it may use per day, by number of requests or by estimated dollars, and see the request that is refused.
---

# Set spending limits

**What you get:** a daily limit for each team and for each person in it. When a limit is used up, the gateway refuses new requests with HTTP 429 until midnight UTC.

A `Budget` belongs to one group. It can limit **requests**, **estimated dollars**, or both. This guide shows both: a request limit for engineering and a dollar limit for sales.

## Before you start

You need:

- the cluster from the [Quick start](../quick-start.md), installed with budgets on (`limits.enabled: true`). [Different models for different teams](models-per-team.md) shows the upgrade command;
- the groups and people of that guide: Alice and Dan in `engineering`, Bob in `sales`;
- the models of that guide, `free-model` for engineering and `sales-model` for sales, and a personal API key for each person ([Connect Claude Code and Cursor](coding-tools.md)). Save the keys in `ALICE_KEY`, `DAN_KEY` and `BOB_KEY`.

Budgets count requests and dollars once a day, in UTC. If you repeat this guide on the same day, the counters keep their values until midnight UTC. Use new limits or wait.

## 1. Limit requests for engineering

Engineering gets 5 requests a day for the whole group, and each person gets 3. The numbers are small only so that you reach them quickly.

```sh
kubectl -n ambit-system apply -f - <<'EOF'
apiVersion: ambitproject.dev/v1alpha1
kind: Budget
metadata: { name: engineering }
spec:
  group: engineering
  groupRequestsPerDay: 5
  userRequestsPerDay: 3
EOF
```

A group has one `Budget`. A person in several budgeted groups is counted against each of them.

## 2. Limit dollars for sales

Dollar limits need a price. Ambit counts each answer as `input tokens × input price + output tokens × output price`, from the price on the `Model`. A model without a price is refused to every group that has a dollar limit.

Add a price to `sales-model`. The prices are dollars per million tokens. They are made up for this guide, because the model is free. Then create the budget:

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
  price: { inputPerMillionTokens: "1", outputPerMillionTokens: "4" }
---
apiVersion: ambitproject.dev/v1alpha1
kind: Budget
metadata: { name: sales }
spec:
  group: sales
  groupDollarsPerDay: "0.01"
  userDollarsPerDay: "0.001"
EOF
kubectl -n ambit-system get budgets.ambitproject.dev
```

Dollar amounts are estimates from list prices. They are not an invoice.

A change to a `Budget` takes up to about a minute to reach the gateway. Wait a minute before the next step.

## Check it works

This function makes one call and prints the start of the answer and the HTTP status.

```sh
ask() { # ask KEY MODEL
  curl -s -w ' HTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
    -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in five words.\"}]}"
}
```

**Per person.** Alice calls four times. The fourth call is over her limit of 3.

```sh
for i in 1 2 3 4; do ask "$ALICE_KEY" nvidia/nemotron-3-super-120b-a12b:free | cut -c1-200; done
```

The first three calls end with `HTTP 200`. The fourth is refused:

```text
 HTTP 429
```

The refusal has no body.

**Per group.** Dan has made no call yet, so his own count is 0. He calls three times:

```sh
for i in 1 2 3; do ask "$DAN_KEY" nvidia/nemotron-3-super-120b-a12b:free | cut -c1-200; done
```

His first call ends with `HTTP 200`. The second and third are refused with `HTTP 429`. The group limit of 5 is used up, because the whole group shares it. Refused requests count against a request limit too, so Alice's refused call used one of the five.

**Dollars.** Bob calls the sales model three times:

```sh
for i in 1 2 3; do ask "$BOB_KEY" nvidia/nemotron-3.5-lightning:free | cut -c1-200; sleep 3; done
```

The first call ends with `HTTP 200`. It costs more than the limit of $0.001 for one person, so the second and third calls are refused with `HTTP 429`. Ambit checks the total before a request and adds the cost after the answer. A call that starts while the person is under the limit finishes and is charged in full.

A refused request has no body, but its headers tell the client how long to wait. `retry-after` is the number of seconds until midnight UTC:

```sh
curl -s -i http://llm.localtest.me:8080/v1/chat/completions -H "Authorization: Bearer $BOB_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3.5-lightning:free","messages":[{"role":"user","content":"hi"}]}' | head -5
```

```text
HTTP/1.1 429 Too Many Requests
x-ratelimit-limit: 100
x-ratelimit-remaining: 0
x-ratelimit-reset: 56493
retry-after: 56493
```

Use `retry-after` to know when the limit ends. The `x-ratelimit-*` numbers come from the rate limit service and are not the numbers of your `Budget`.

## See the use in the console

Open **Budgets** as an admin. Each budget shows today's requests or dollars, for the group and for each person, and the time the counters restart. A refused request is recorded in **Audit** like any other call.

You can also see the same numbers with the console API, signed in as an admin: `GET /api/budgets`.

## Related

- [Budgets](../budgets.md): what is counted, streams, models without a price, and how prices are kept up to date.
- [Different models for different teams](models-per-team.md): the groups and models used here.
- [Alerts](../alerts.md): alerts for the budget store.
