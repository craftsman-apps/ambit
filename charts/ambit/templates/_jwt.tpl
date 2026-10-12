{{/*
ambit.jwt: the token settings of one gateway (llm.jwt or mcp.jwt) as JSON. With dex: true the gateway
accepts tokens from Ambit's own Dex (client ambit-cli), and reads Dex's keys inside the cluster.
Usage: fromJson (include "ambit.jwt" (dict "root" $ "jwt" .Values.llm.jwt "name" "llm"))
*/}}
{{- define "ambit.jwt" -}}
{{- $j := .jwt -}}
{{- if $j.dex -}}
{{- if or $j.issuer $j.jwksURL $j.audiences -}}
{{- fail (printf "%s.jwt.dex=true sets the issuer itself: leave %s.jwt.issuer, audiences and jwksURL empty" .name .name) -}}
{{- end -}}
{{- toJson (dict "issuer" (printf "%s/dex" .root.Values.console.url) "audiences" (list "ambit-cli") "jwksURL" (printf "http://ambit-dex.%s.svc:5556/dex/keys" .root.Release.Namespace) "requireEmail" (ne (toString $j.requireEmail) "false")) -}}
{{- else -}}
{{- toJson (merge (dict "requireEmail" (ne (toString $j.requireEmail) "false")) (omit $j "requireEmail")) -}}
{{- end -}}
{{- end -}}

{{- define "ambit.dexIssuesGatewayTokens" -}}
{{- if or (and .Values.llm.enabled .Values.llm.jwt.dex) (and .Values.mcp.enabled .Values.mcp.jwt.dex) }}true{{ end -}}
{{- end -}}
