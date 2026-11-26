{{- /* The bucket of a request on the public Gateway, as a CEL expression for the source limit (policy ambit-public-source-limit, docs/front-door.md,
"Rate limits"). Separate buckets, so that a flood of one kind never uses up the limit of another: mcp (the default: calls with a key or a token, and
every request that matches nothing), discovery (the metadata documents, the key list, and a call to the MCP URL without any credential: the 401 that
starts a sign-in), signin (the pages of the sign-in host), token (Hydra's token and revoke), registration. The bucket reads the path and the presence of two
headers only; the source of the count is never read from a header. Without OAuth every request is "mcp". */ -}}
{{- define "ambit.frontdoor.bucket" -}}
{{- if (.Values.oauth | default dict).enabled -}}
request.path == "/oauth2/token" || request.path == "/oauth2/revoke" ? "token" : request.path == "/register" ? "registration" : request.path in ["/oauth2/auth", "/login", "/callback", "/consent", "/error"] ? "signin" : request.path.startsWith("/.well-known/") ? "discovery" : (request.path == "/mcp" && !("x-api-key" in request.headers) && !("authorization" in request.headers)) ? "discovery" : "mcp"
{{- else -}}
"mcp"
{{- end -}}
{{- end -}}
