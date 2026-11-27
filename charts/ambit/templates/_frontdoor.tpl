{{- /* The bucket of a request on the public Gateway, as a CEL expression for the source limit (policy ambit-public-source-limit, docs/front-door.md,
"Rate limits"). Separate buckets, so that a flood of one kind never uses up the limit of another: mcp (the default: calls with a key or a token, and
everything that is not in a bucket below), discovery (the metadata documents, the key list, and a call to the MCP URL without any credential: the 401 that
starts a sign-in), signin (the pages of the sign-in host, and every path of that host that is not one of the others), token (Hydra's token and revoke),
registration, and bearer (calls to the MCP URL that carry a bearer token that is not an Ambit key: see below). The bucket reads the HOST, the path and two headers only; the source of the count is never read from a header. A path of
the sign-in host in a request to the MCP host (where it is a 404) is in "mcp", so a flood on one host cannot spread over the buckets of the other
(security review N8). Without OAuth every request is "mcp".
The bucket "bearer" (#436) is for the calls of hosted platforms, which send the calls of many people from a few addresses: a request to the MCP URL with NO x-api-key
and an Authorization header that is, as a whole, "Bearer" and a token in the shape of a JWT (three segments of base64url characters, the first starting with
eyJ, which is the start of every JSON header). The shape is the security point: the bucket is chosen BEFORE the token is checked, so a caller that sends a
value that the key rule could take for a key must never get the higher limit. The key rule reads "Bearer " off the header with a replace of every
occurrence, so "Bearer Bearer ambit_..." is a key: it has a space, it is not in the shape, and it stays in "mcp" (key guessing is limited as before). A
value in the shape can never be a key (no key starts with eyJ). A caller can still send junk in the shape: it gets the 401 of the JWT check, counts in this bucket
only (it never uses up the "mcp" limit of the same address), and costs the gateway no more than a parse and a signature check (docs/front-door.md,
"Rate limits"). */ -}}
{{- define "ambit.frontdoor.bucket" -}}
{{- if (.Values.oauth | default dict).enabled -}}
{{- $auth := .Values.frontDoor.authHost -}}
{{- $mcp := .Values.frontDoor.host -}}
{{- $onAuth := printf "(request.host == %q || request.host.startsWith(%q))" $auth (printf "%s:" $auth) -}}
{{- $onMcp := printf "(request.host == %q || request.host.startsWith(%q))" $mcp (printf "%s:" $mcp) -}}
{{ $onAuth }} ? (request.path == "/oauth2/token" || request.path == "/oauth2/revoke" ? "token" : request.path == "/register" ? "registration" : request.path.startsWith("/.well-known/") ? "discovery" : "signin") : {{ $onMcp }} && (request.path.startsWith("/.well-known/") || (request.path == "/mcp" && !("x-api-key" in request.headers) && !("authorization" in request.headers))) ? "discovery" : {{ $onMcp }} && request.path == "/mcp" && !("x-api-key" in request.headers) && "authorization" in request.headers && request.headers["authorization"].matches("^Bearer eyJ[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]*$") ? "bearer" : "mcp"
{{- else -}}
"mcp"
{{- end -}}
{{- end -}}
