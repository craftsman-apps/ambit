{{/*
CEL fragments of who a gateway request belongs to. They must equal internal/identity/identity.go
(internal/identity/chart_test.go decodes the rendered policies and compares). Emit them with `quote`.
The helpers that depend on the email rule take (dict "requireEmail" <bool>); the others take anything.
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
{{- if .requireEmail -}}has(jwt.sub) && {{ include "ambit.identity.hasEmail" . }}{{- else -}}has(jwt.sub){{- end -}}
{{- end -}}
{{- define "ambit.identity.gate" -}}
({{ include "ambit.identity.presented" . }}) ? ({{ include "ambit.identity.validKey" . }}) : {{ if .requireEmail }}({{ include "ambit.identity.jwtGate" . }}){{ else }}{{ include "ambit.identity.jwtGate" . }}{{ end }}
{{- end -}}
{{- define "ambit.identity.groups" -}}
(has(apiKey.keyId) ? apiKey.groups : (has(jwt.groups) ? jwt.groups : []))
{{- end -}}
{{- define "ambit.identity.email" -}}
(has(apiKey.keyId) ? apiKey.email : (({{ include "ambit.identity.hasEmail" . }}) ? jwt.email.trim().lowerAscii() : ""))
{{- end -}}
{{- define "ambit.identity.principal" -}}
{{- if .requireEmail -}}(has(apiKey.keyId) ? apiKey.email : jwt.email.trim().lowerAscii()){{- else -}}(has(apiKey.keyId) ? apiKey.email : (({{ include "ambit.identity.hasEmail" . }}) ? jwt.email.trim().lowerAscii() : jwt.sub)){{- end -}}
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
      - {{ include (ternary "ambit.identity.gate" "ambit.identity.jwtGate" $keys) (dict "requireEmail" .jwt.requireEmail) | quote }}
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
