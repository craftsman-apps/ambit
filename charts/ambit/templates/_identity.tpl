{{/*
CEL fragments of who a gateway request belongs to. They must equal internal/identity/identity.go
(internal/identity/chart_test.go decodes the rendered policies and compares). Emit them with `quote`.
The helpers that depend on the email rule take (dict "requireEmail" <bool>); ambit.identity.email takes
(dict "verified" <bool>) (accessRules.usersRequireVerifiedEmail); the others take anything.
*/}}
{{- define "ambit.identity.hasEmail" -}}
has(jwt.email) && type(jwt.email) == string && jwt.email.trim().matches('^[^@\\s]+@[^@\\s]+$')
{{- end -}}
{{- define "ambit.identity.presented" -}}
"x-api-key" in request.headers || ("authorization" in request.headers && request.headers["authorization"].startsWith("Bearer ambit_"))
{{- end -}}
{{- define "ambit.identity.validKey" -}}
has(apiKey.keyId) && timestamp(apiKey.until) > request.startTime
{{- end -}}
{{- define "ambit.identity.jwtGate" -}}
{{- if .requireEmail -}}has(jwt.sub) && {{ include "ambit.identity.hasEmail" . }} && !jwt.email.trim().lowerAscii().endsWith("@svc.ambit.internal")
{{- else -}}has(jwt.sub) && !jwt.sub.trim().lowerAscii().endsWith("@svc.ambit.internal") && !({{ include "ambit.identity.hasEmail" . }} && jwt.email.trim().lowerAscii().endsWith("@svc.ambit.internal")){{- end -}}
{{- end -}}
{{/* (dict "requireEmail" <bool> "services" <bool>): services false (serviceAccounts.enabled) refuses service account keys */}}
{{- define "ambit.identity.gate" -}}
({{ include "ambit.identity.presented" . }}) ? ({{ include "ambit.identity.validKey" . }}{{ if not .services }} && !apiKey.email.endsWith("@svc.ambit.internal"){{ end }}) : ({{ include "ambit.identity.jwtGate" . }})
{{- end -}}
{{- define "ambit.identity.kind" -}}
({{ include "ambit.identity.validKey" . }}) ? (apiKey.email.endsWith("@svc.ambit.internal") ? "service account" : "key") : (({{ include "ambit.identity.presented" . }}) ? "invalid key" : "jwt")
{{- end -}}
{{- define "ambit.identity.groups" -}}
(has(apiKey.keyId) ? apiKey.groups : (has(jwt.groups) ? jwt.groups : []))
{{- end -}}
{{- define "ambit.identity.email" -}}
(has(apiKey.keyId) ? apiKey.email : (({{ include "ambit.identity.hasEmail" . }}{{ if .verified }} && has(jwt.email_verified) && jwt.email_verified == true{{ end }}) ? jwt.email.trim().lowerAscii() : ""))
{{- end -}}
{{/* The caller as the access log records it (jwt.email, ambit.key_id); the guard's compression records use the same. */}}
{{- define "ambit.identity.logEmail" -}}
({{ include "ambit.identity.validKey" . }}) ? apiKey.email : (({{ include "ambit.identity.presented" . }}) ? "" : (has(jwt.email) ? jwt.email.trim().lowerAscii() : ""))
{{- end -}}
{{- define "ambit.identity.logKeyId" -}}
({{ include "ambit.identity.validKey" . }}) ? apiKey.keyId : ""
{{- end -}}
{{/* The call ID ambit-guard returns for each tools/call (#203); "" for other requests. */}}
{{- define "ambit.callId" -}}
has(mcpGuardrails.callId) ? mcpGuardrails.callId : ""
{{- end -}}
{{- define "ambit.identity.principal" -}}
{{- if .requireEmail -}}(has(apiKey.keyId) ? apiKey.email : jwt.email.trim().lowerAscii()){{- else -}}(has(apiKey.keyId) ? apiKey.email : (({{ include "ambit.identity.hasEmail" . }}) ? jwt.email.trim().lowerAscii() : jwt.sub)){{- end -}}
{{- end -}}

{{/* The issuer whose tokens may carry agent claims: agents.tokenIssuer, or, with OAuth sign-in, Ambit's own sign-in host. "" before OAuth. */}}
{{- define "ambit.agents.tokenIssuer" -}}
{{- $a := .Values.agents | default dict -}}
{{- if $a.tokenIssuer -}}{{ $a.tokenIssuer }}{{- else if (.Values.oauth | default dict).enabled -}}https://{{ .Values.frontDoor.authHost }}{{- end -}}
{{- end -}}

{{/* Agents (identity.AgentAttr, identity.TokenIssuer, identity.SourceRanges): one audit attribute of an agent call, read only from
a verified credential. Takes (dict "key" <metadata field or ""> "claim" <token claim or ""> "root" $); .root.Values.agents.tokenIssuer is the issuer
whose tokens may carry agent claims ("" in 0.6.0 before OAuth). */}}
{{- define "ambit.identity.agentAttr" -}}
{{- $iss := include "ambit.agents.tokenIssuer" .root -}}
{{- $jwt := `""` -}}
{{- if and .claim $iss -}}{{- $jwt = printf `((has(jwt.iss) && jwt.iss == %s && has(jwt.%s)) ? jwt.%s : "")` ($iss | toJson) .claim .claim -}}{{- end -}}
{{- if .key -}}(({{ include "ambit.identity.validKey" . }}) && has(apiKey.{{ .key }})) ? apiKey.{{ .key }} : {{ $jwt }}{{- else -}}{{ $jwt }}{{- end -}}
{{- end -}}
{{- define "ambit.identity.tokenIssuer" -}}
(({{ include "ambit.identity.presented" . }}) || !has(jwt.iss)) ? "" : jwt.iss
{{- end -}}
{{- define "ambit.identity.sourceRanges" -}}
!has(apiKey.sourceRanges) || apiKey.sourceRanges.exists(r, cidr(r).containsIP(source.address))
{{- end -}}

{{/*
The auth policy body shared by the LLM and MCP gateways. Usage:
include "ambit.gatewayAuth" (dict "root" $ "jwt" $jwt "scope" "llm" "extra" (list ...))
*/}}
{{- define "ambit.gatewayAuth" -}}
{{- $keys := .root.Values.apiKeys.enabled -}}
authorization:
  action: Require
  policy:
    matchExpressions:
      {{- range .extra }}
      - {{ . | quote }}
      {{- end }}
      {{- if and $keys (eq .scope "mcp") }}
      # An agent key may name source ranges (at most 10 CIDRs): it is accepted only from them.
      - {{ include "ambit.identity.sourceRanges" . | quote }}
      {{- end }}
      - {{ include (ternary "ambit.identity.gate" "ambit.identity.jwtGate" $keys) (dict "requireEmail" .jwt.requireEmail "services" (ne (toString (.root.Values.serviceAccounts | default dict).enabled) "false")) | quote }}
jwtAuthentication:
  mode: {{ ternary "Permissive" "Strict" $keys }}
  providers:
    - issuer: {{ .jwt.issuer | quote }}
      {{- with .jwt.audiences }}
      audiences: {{ toJson . }}
      {{- end }}
      jwks:
        remote:
          url: {{ .jwt.jwksURL | quote }}
{{- if $keys }}
apiKeyAuthentication:
  mode: Permissive
  secretSelector:
    matchLabels: { ambitproject.dev/key: "true", ambitproject.dev/key-{{ .scope }}: "true" }
  location:
    expression: '"x-api-key" in request.headers ? request.headers["x-api-key"] : request.headers["authorization"].replace("Bearer ", "")'
{{- end }}
{{- end -}}
