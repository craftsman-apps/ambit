{{- /* The bucket of a request on the public Gateway, as a CEL expression for the source limit (policy ambit-public-source-limit, docs/front-door.md,
"Rate limits"). Separate buckets, so that a flood of one kind never uses up the limit of another: mcp (the default: calls with a key or a token, and
everything that is not in a bucket below), discovery (the metadata documents, the key list, and a call to the MCP URL without any credential: the 401 that
starts a sign-in), signin (the pages of the sign-in host, and every path of that host that is not one of the others), token (Hydra's token and revoke),
registration. The bucket reads the HOST and the path and the presence of two headers only; the source of the count is never read from a header. A path of
the sign-in host in a request to the MCP host (where it is a 404) is in "mcp", so a flood on one host cannot spread over the buckets of the other
(security review N8). Without OAuth every request is "mcp". */ -}}
{{- define "ambit.frontdoor.bucket" -}}
{{- if (.Values.oauth | default dict).enabled -}}
{{- $auth := .Values.frontDoor.authHost -}}
{{- $mcp := .Values.frontDoor.host -}}
{{- $onAuth := printf "(request.host == %q || request.host.startsWith(%q))" $auth (printf "%s:" $auth) -}}
{{- $onMcp := printf "(request.host == %q || request.host.startsWith(%q))" $mcp (printf "%s:" $mcp) -}}
{{ $onAuth }} ? (request.path == "/oauth2/token" || request.path == "/oauth2/revoke" ? "token" : request.path == "/register" ? "registration" : request.path.startsWith("/.well-known/") ? "discovery" : "signin") : {{ $onMcp }} && (request.path.startsWith("/.well-known/") || (request.path == "/mcp" && !("x-api-key" in request.headers) && !("authorization" in request.headers))) ? "discovery" : "mcp"
{{- else -}}
"mcp"
{{- end -}}
{{- end -}}
