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
- Routes with a `category` need a classifier (types VSR and LLM, a later release). With `None` or `Rules` they
  are ignored, and the router reports `CategoryNeedsClassifier`.

## Budgets and audit

Budgets charge the Model that answered, at its price, including streamed answers whose client did not ask for
token counts. The Audit page shows routed calls as `auto → claude-sonnet`, and the export carries the same text.

## Conditions

`kubectl get modelrouters` shows each router's `Ready` condition:

| Reason | Meaning | What happens |
|---|---|---|
| `Accepted` | the gateway serves the router | — |
| `NameTaken` | a Model (exactly or by a wildcard such as `gpt-*`) or another router already serves this name, or a Model is named `router-<name>` | the router is not served; rename one |
| `UnknownDefault` | no Model with the default's name | the router is not served |
| `AmbiguousTarget` | a route's Model serves a wildcard, or another Model serves the same name | the router is not served |
| `TooManyRoutes` | more than 16 routes or 32 keywords | the router is not served |
| `UnknownModel` | a route names a Model that does not exist | that route is dropped, the rest is served |
| `CategoryNeedsClassifier` | a route has a category but the type is None or Rules | that route is ignored, the rest is served |
| `DefaultNotAllowed` | no group may use both the router and its default | served, but its users get 400 when no route they may use matches |
