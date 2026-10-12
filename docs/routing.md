# Model routing: one model name, the right Model per request

A `ModelRouter` is a model name clients can ask for, such as `auto`. For each request the gateway picks one of
your Models by ordered **routes**, and it only ever picks a Model the user may use. The user's tool sends
`"model": "auto"` as with any other model; nothing else changes for it.

```yaml
apiVersion: ambitproject.dev/v1alpha1
kind: ModelRouter
metadata: { name: auto }          # clients send "model": "auto" (or set spec.model to another name)
spec:
  default: gemma                  # the Model that answers when no route matches
  classifier: { type: Rules }     # None: always the default; Rules: use the routes below
  routes:                         # tried in order, at most 16
    - model: claude-sonnet
      keywords: [python, traceback, "def "]   # any of them, ASCII case-insensitive
    - model: gemini-pro
      minBytes: 20000             # long prompts
---
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: eng-auto }
spec:
  groups: [eng]
  models: [auto]                  # a router is granted like a Model
```

## How a Model is picked

Each route becomes one line, and the gateway takes the first line that is true:

1. the user may use the router (an AccessRule lists it), **and**
2. the request is `/v1/chat/completions`, **and**
3. the route's conditions hold (all that are set: `keywords`, `minBytes`, `maxBytes`), **and**
4. the user may use that route's Model (its AccessRules, and the refusal of unpriced Models to groups with a
   dollar limit).

The last line is the default: the user may use the router and the default Model. Because each line checks the
Model's access, a user who may not use `claude-sonnet` simply falls through to the next line or the default;
they never get an error for it. A user who matches no line, for example because they may not use the router
or its default, gets HTTP 400 `virtual_model_no_matching_target`.

## What the rules look at

- The **last user message**, when its content is plain text. A message made of parts (text plus an image, for
  example) does not match keyword or length routes; such requests get the default.
- **Keywords**: ASCII text, 1 to 100 characters each, at most 32 per router. Matching ignores case for ASCII
  letters only (`Python` matches `python`; `Größe` is not folded).
- **Length** in UTF-8 bytes (`minBytes`, `maxBytes`). An English letter is one byte; many other characters are
  two to four.
- Only `/v1/chat/completions` is routed. A request for `auto` on `/v1/messages` or `/v1/responses` goes to the
  default.
- Routes with a `category` need a classifier (type `LLM`). With `None` or
  `Rules` they are ignored, and the router reports `CategoryNeedsClassifier`.

## Type LLM: a small model picks the category

With `classifier.type: LLM`, a small LLM reads each chat prompt and answers one of the router's categories.
Routes with a `category` match on that answer; keyword and length routes keep working beside them.

```yaml
apiVersion: ambitproject.dev/v1alpha1
kind: ModelRouter
metadata: { name: auto }
spec:
  default: gemma
  classifier:
    type: LLM
    llm: { model: qwen-local }        # a Model with an OpenAI-compatible endpoint
    timeout: 1500ms                   # default 1500ms, at most 10s
    maxBytes: 8000                    # text sent to the classifier, default 8000
    ratePerMinute: 600                # classifier calls per ambit-router replica, default 600
    categories:
      - { name: coding, description: "writing, fixing or explaining program code" }
  routes:
    - { model: claude-sonnet, category: coding }
    - { model: gemini-pro, minBytes: 20000 }
```

- **The classifier Model** needs an OpenAI-compatible endpoint: a `baseURL` (vLLM, Ollama, LiteLLM …), or
  provider `OpenAI` or `Openrouter`. Otherwise the router reports `ClassifierModelInvalid`.
- **What it sees**: the last user message (plain text only), cut to `maxBytes` on a character boundary, with a
  system prompt that lists the categories. It answers one name; anything else is no label.
- **Large requests**: a request body over 4,000,000 bytes is not sent to the classifier and gets no label
  (keyword and length lines can still match, otherwise the default answers).
- **Where the text goes**: to the classifier Model's provider. Use a model in your cluster for sensitive prompts,
  and in air-gapped clusters.
- **Cost**: the classifier's tokens are not charged to budgets. They are counted in the metric
  `ambit_router_classifier_tokens_total{router,model}`; spending is bounded by the router's access rules and
  `ratePerMinute`.
- **Steering**: a prompt can try to steer its own label ("answer coding"). It can only choose among models the
  user may use anyway.
- **A category name means the same in every router**: two routers that describe `coding` differently get
  `CategoryConflict`. The name `other` is reserved.

### ambit-router

The classifier is called by `ambit-router`, a small service installed with Ambit (`routing.router.enabled`,
default true when LLM traffic is on). It runs only while a router of type `LLM` exists: the controller scales it
to `routing.router.replicas` (default 2) and back to 0, and `kubectl get modelrouters` shows its state
(`Classifier State`: `Off`, `Starting`, `Ready` or `Unhealthy`). The Dependencies page shows it too.

Every request gets one reason, in the access log and on the Audit page as `auto → model (reason or label)`:

| Reason | Meaning |
|---|---|
| `classified` | the classifier answered one of the router's categories (the label is shown instead) |
| `unknown-category` | it answered something else, or `other` |
| `timeout` | no answer within `timeout` |
| `error` | the call failed, or the message is not plain text |
| `busy` | 16 classifications were already running on that replica |
| `rate-limited` | the router's `ratePerMinute` is used up on that replica |
| `denied` | the user may not use the router (no call is made; the gateway answers 400) |
| `stale` | ambit-router has not loaded the configuration the gateway names yet (usually for about a second after a change) |

**A failure never fails the request**: with no label, keyword and length routes still apply, and otherwise the
default answers. If no ambit-router pod is reachable, the gateway skips it (no label). Two cases can fail a
request: ambit-router crashing while it holds that request (500), and, during a rollout or scale-down, a request
whose body upload takes longer than about 23 seconds.

**Changes take effect without restarts.** A router change reaches the gateway in about 200 ms and ambit-router
through its watch; in between, requests get `stale` (no label), never a label from the old configuration. A
removed user can have prompts classified for that moment, like any access change. A rotated classifier key is
used within about a minute (21 s measured on kind).

**Permissions.** To copy a classifier Model's key into `ambit-router-credentials`, the controller may read any
Secret in Ambit's namespace (by name, never listing them) and create Secrets there. ambit-router itself may read
only its own ConfigMap and that one Secret.

**Argo CD.** The chart leaves `ambit-router`'s replicas to the controller. Tell Argo CD to ignore them, or every
sync would scale it back:

```yaml
spec:
  ignoreDifferences:
    - group: apps
      kind: Deployment
      name: ambit-router
      jsonPointers: [/spec/replicas]
  syncPolicy:
    syncOptions: [RespectIgnoreDifferences=true]
```

## Budgets and audit

Budgets charge the Model that answered, at its price, including streamed answers whose client did not ask for
token counts. The Audit page shows routed calls as `auto → claude-sonnet`, with the label or reason for type
LLM (`auto → claude-sonnet (coding)`, `auto → gemma (timeout)`), and the export carries the same text.

## Conditions

`kubectl get modelrouters` shows each router's `Ready` condition:

| Reason | Meaning | What happens |
|---|---|---|
| `Accepted` | the gateway serves the router | — |
| `NameTaken` | a Model (exactly or by a wildcard such as `gpt-*`) or another router already serves this name, or a Model has the same object name as the router (AccessRules could not tell them apart), or a Model is named `router-<name>` | the router is not served; rename one |
| `UnknownDefault` | no Model with the default's name | the router is not served |
| `AmbiguousTarget` | a route's Model serves a wildcard, or another Model serves the same name | the router is not served |
| `TooManyRoutes` | more than 16 routes or 32 keywords | the router is not served |
| `UnknownModel` | a route names a Model that does not exist | that route is dropped, the rest is served |
| `CategoryNeedsClassifier` | a route has a category but the type is None or Rules | that route is ignored, the rest is served |
| `UnknownCategory` | a route's category is not in `classifier.categories` | that route is dropped, the rest is served |
| `CategoryConflict` | another router describes a category of the same name differently | the router is not served |
| `ClassifierModelInvalid` | the classifier Model is missing, has no OpenAI-compatible endpoint, or its Secret has no `Authorization` entry | the router is not served |
| `ClassifierNotInstalled` | type LLM, but ambit-router is not installed (`routing.router.enabled=false`) | category routes are ignored, the rest is served |
| `ClassifierUnhealthy` | some ambit-router replicas are not available | served; some requests may get no label |
| `ClassifierStartFailed` | ambit-router has not become available in 15 minutes | served without labels; see `kubectl describe deployment ambit-router` |
| `DefaultNotAllowed` | no group may use both the router and its default | served, but its users get 400 when no route they may use matches |
