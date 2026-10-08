package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ModelSpec is one entry in the model catalog: a model clients may request through the gateway.
// +kubebuilder:validation:XValidation:rule="!has(self.formats) || (has(self.baseURL) && self.provider == 'OpenAI')",message="formats describe a self-hosted server: they need provider OpenAI and a baseURL"
type ModelSpec struct {
	// Provider serving the model. Values follow agentgateway's AgentgatewayModel providers that need only an API key.
	// Azure, Bedrock and VertexAI are not offered yet: agentgateway needs provider settings and cloud credentials
	// for them that a Model cannot express yet. Ollama and other self-hosted servers: provider OpenAI with a baseURL.
	// +kubebuilder:validation:Enum=Anthropic;Baseten;Cerebras;Cohere;Deepinfra;Deepseek;Fireworks;Gemini;Groq;Huggingface;Meta;Mistral;OpenAI;Openrouter;TogetherAI;XAI
	Provider string `json:"provider"`
	// Model is the exact name clients send in the request, e.g. "google/gemma-4-31b-it:free".
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// CredentialsRef names a Secret in this namespace with the provider key in its "Authorization" entry.
	// The key itself never appears in this object, so the object is safe to keep in Git. Only Secrets
	// named ambit-llm-* may be used, so a Model cannot send another Secret (e.g. console keys) to a provider.
	// +kubebuilder:validation:XValidation:rule="self.name.startsWith('ambit-llm-')",message="credentialsRef must name a Secret called ambit-llm-<something>"
	CredentialsRef SecretRef `json:"credentialsRef"`
	// DisplayName is shown in the console.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// BaseURL of a self-hosted, OpenAI-compatible server (vLLM, Ollama, LiteLLM ...), with provider
	// OpenAI, e.g. http://vllm.models.svc:8000/v1. Include the API path (usually /v1): a URL without a path
	// sends requests to / (agentgateway v1.6.0). Without it, the provider's own endpoint is used.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://[^/?#\s]+(/[^?#\s]*)?$`
	BaseURL string `json:"baseURL,omitempty"`
	// Formats are the APIs the server at baseURL implements (provider OpenAI only); agentgateway converts requests
	// in other formats to one of these. Default: Completions and Embeddings. Before this field existed, requests
	// for /v1/responses and /v1/messages were passed to the server unchanged; list Responses or Messages to keep
	// that when the server implements them natively (for example LiteLLM, or vLLM for Responses). A classifier
	// Model (ModelRouter type LLM) needs Completions.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	Formats []ModelFormat `json:"formats,omitempty"`
	// Price of this model, for cost in Usage. Without it, Usage shows tokens only for this model.
	// +optional
	Price *ModelPrice `json:"price,omitempty"`
}

// +kubebuilder:validation:Enum=Completions;Messages;Responses;Embeddings
type ModelFormat string

// ModelPrice is what the provider charges, in US dollars per million tokens.
type ModelPrice struct {
	// +kubebuilder:validation:Pattern=`^[0-9]{1,6}(\.[0-9]{1,6})?$`
	InputPerMillionTokens string `json:"inputPerMillionTokens"`
	// +kubebuilder:validation:Pattern=`^[0-9]{1,6}(\.[0-9]{1,6})?$`
	OutputPerMillionTokens string `json:"outputPerMillionTokens"`
}

type SecretRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type ModelStatus struct {
	// Who may use this model, compiled from every AccessRule that names it. Empty means nobody.
	// +optional
	AllowedGroups []string `json:"allowedGroups,omitempty"`
	// +optional
	AllowAllSignedIn bool `json:"allowAllSignedIn,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// Model is a model in the Ambit catalog. Nobody may use it until an AccessRule names it.
type Model struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelSpec   `json:"spec"`
	Status            ModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

// AccessRuleSpec says who may use which models and MCP tools. Rules only grant access; nothing is allowed by default.
// +kubebuilder:validation:XValidation:rule="(has(self.groups) && size(self.groups) > 0) != (has(self.allSignedIn) && self.allSignedIn)",message="set either groups or allSignedIn: true, not both"
// +kubebuilder:validation:XValidation:rule="(has(self.models) && size(self.models) > 0) || (has(self.servers) && size(self.servers) > 0)",message="name at least one model or server"
type AccessRuleSpec struct {
	// Groups from the identity provider's "groups" claim. A member of any listed group is allowed.
	// +optional
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9][A-Za-z0-9._@/-]*$`
	Groups []string `json:"groups,omitempty"`
	// AllSignedIn allows everyone with a valid token.
	// +optional
	AllSignedIn bool `json:"allSignedIn,omitempty"`
	// Models lists Model object names in this namespace.
	// +optional
	Models []string `json:"models,omitempty"`
	// Servers lists Server objects in this namespace and, optionally, which of their tools are allowed.
	// +optional
	Servers []ServerAccess `json:"servers,omitempty"`
}

type ServerAccess struct {
	// Name of a Server object in this namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Tools allowed on this server, by the name the server gives them (without the gateway's
	// "<server>_" prefix). Omit to allow every tool the server offers, including tools it adds later.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9_.-]{1,128}$`
	Tools []string `json:"tools,omitempty"`
	// Arguments limit what the listed tools may be called with (requirement 8): every condition must
	// hold, or ambit-guard refuses the call and says which condition failed. Needs protection enabled;
	// without it, a rule with argument limits grants nothing.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Arguments []ArgumentRule `json:"arguments,omitempty"`
}

// ArgumentRule is one condition on one top-level argument of a tool call.
// +kubebuilder:validation:XValidation:rule="has(self.oneOf) || has(self.notContains) || has(self.max)",message="set oneOf, notContains or max"
type ArgumentRule struct {
	// Name of the argument, as the tool's input schema names it (top level only).
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`
	Name string `json:"name"`
	// OneOf: the argument must be present and equal to one of these strings.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=256
	OneOf []string `json:"oneOf,omitempty"`
	// NotContains: when present, the argument (as text) must not contain any of these, ignoring case.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	NotContains []string `json:"notContains,omitempty"`
	// Max: the argument must be present, a number, and at most this value, e.g. "1000" or "99.5".
	// +optional
	// +kubebuilder:validation:Pattern=`^-?[0-9]{1,15}(\.[0-9]{1,6})?$`
	Max string `json:"max,omitempty"`
}

type AccessRuleStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Groups",type=string,JSONPath=`.spec.groups`
// +kubebuilder:printcolumn:name="All signed in",type=boolean,JSONPath=`.spec.allSignedIn`
// +kubebuilder:printcolumn:name="Models",type=string,JSONPath=`.spec.models`
// +kubebuilder:printcolumn:name="Servers",type=string,JSONPath=`.spec.servers`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// AccessRule grants groups (or everyone signed in) the use of models and MCP tools.
type AccessRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AccessRuleSpec   `json:"spec"`
	Status            AccessRuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AccessRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AccessRule `json:"items"`
}

// ServerSpec is an MCP server in the Ambit catalog: an image that ToolHive runs in a pod, or a remote
// server at a URL. Clients reach its tools only through the gateway, as "<server name>_<tool>".
// +kubebuilder:validation:XValidation:rule="has(self.image) != has(self.url)",message="set either image (run by ToolHive) or url (a remote server), not both"
// +kubebuilder:validation:XValidation:rule="!has(self.connection) || has(self.url)",message="connection needs a remote server (url): a per-user token cannot reach a stdio server"
type ServerSpec struct {
	// Image pinned by digest, e.g. ghcr.io/org/server@sha256:<64 hex>. A tag can later point to
	// different code; a digest cannot, so what was reviewed is what runs.
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[a-f0-9]{64}$`
	// +optional
	Image string `json:"image,omitempty"`
	// URL of a remote MCP server (streamable HTTP or SSE), e.g. https://api.githubcopilot.com/mcp/.
	// +kubebuilder:validation:Pattern=`^https?://[^/?#]+(/[^?#]*)?$`
	// +optional
	URL string `json:"url,omitempty"`
	// Connection names a Connection: each call then carries the calling user's own token for that
	// app, and a user without one gets a link to connect it.
	// +optional
	Connection string `json:"connection,omitempty"`
	// How the server itself talks MCP. stdio servers are wrapped by ToolHive's proxy.
	// +kubebuilder:validation:Enum=stdio;streamable-http;sse
	// +kubebuilder:default=stdio
	// +optional
	Transport string `json:"transport,omitempty"`
	// Port the server listens on, for streamable-http and sse servers.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	MCPPort int32 `json:"mcpPort,omitempty"`
	// DisplayName is shown in the console.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
}

type ServerStatus struct {
	// Tools the running server offers, as it names them (clients see "<server>_<tool>").
	// Read by ambit-controller through ToolHive's proxy; empty until the server is running.
	// +optional
	Tools []Tool `json:"tools,omitempty"`
	// ApprovedTools are the pinned tool definitions (journey A6). Pinned when first seen; a tool whose
	// definition differs, or a new tool, is hidden and refused until an admin approves it again.
	// +optional
	ApprovedTools []ToolPin `json:"approvedTools,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type Tool struct {
	Name string `json:"name"`
	// +optional
	Description string `json:"description,omitempty"`
	// Fingerprint is sha256 over the name, description and input schema.
	// +optional
	Fingerprint string `json:"fingerprint,omitempty"`
	// ReadOnlyHint and DestructiveHint are the server's own MCP annotations, shown as hints only: a
	// server's claims about itself are not trusted.
	// +optional
	ReadOnlyHint *bool `json:"readOnlyHint,omitempty"`
	// +optional
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
}

type ToolPin struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// The name becomes the tool prefix and part of a Service name, so it must be short and DNS-safe.
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 40",message="name must be lowercase letters, digits and dashes, at most 40 characters"

// Server is an MCP server in the Ambit catalog. Nobody may use its tools until an AccessRule names it.
type Server struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ServerSpec   `json:"spec"`
	Status            ServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Server `json:"items"`
}

// ConnectionSpec is a SaaS app users connect with their own account (OAuth 2.0 authorization code).
// Ambit keeps each user's grant, refreshes it, and revokes it at the provider on disconnect or offboarding.
type ConnectionSpec struct {
	// DisplayName is shown on the user's "My connections" page, e.g. "Jira".
	// +kubebuilder:validation:MinLength=1
	DisplayName      string `json:"displayName"`
	AuthorizationURL string `json:"authorizationURL"`
	TokenURL         string `json:"tokenURL"`
	// RevocationURL (RFC 7009) lets Ambit revoke a grant at the provider. Without it, disconnecting
	// only forgets the grant, and the console says so.
	// +optional
	RevocationURL string `json:"revocationURL,omitempty"`
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`
	// ClientSecretRef names a Secret in this namespace with key "clientSecret". Only Secrets named
	// ambit-conn-* may be used.
	// +kubebuilder:validation:XValidation:rule="self.name.startsWith('ambit-conn-')",message="clientSecretRef must name a Secret called ambit-conn-<something>"
	ClientSecretRef SecretRef `json:"clientSecretRef"`
	// +optional
	Scopes []string `json:"scopes,omitempty"`
	// ServerURL binds the Connection to one MCP server: only a Server whose url equals it may name this
	// Connection, so a user's token for this app never goes anywhere else. Empty: no binding.
	// +kubebuilder:validation:Pattern=`^https?://[^/?#]+(/[^?#]*)?$`
	// +optional
	ServerURL string `json:"serverURL,omitempty"`
	// Resource is the RFC 8707 resource indicator sent on authorize, token and refresh requests. Empty: not sent.
	// +kubebuilder:validation:Pattern=`^https?://[^?#]+$`
	// +optional
	Resource string `json:"resource,omitempty"`
	// AuthorizeParams are extra query parameters for the authorize request, e.g. prompt=consent.
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="self.all(k, !(k in ['client_id','redirect_uri','state','code_challenge','code_challenge_method','scope','resource','response_type','response_mode','request','request_uri']))",message="these parameters are set by Ambit"
	// +optional
	AuthorizeParams map[string]string `json:"authorizeParams,omitempty"`
	// TokenRequestFormat is how token requests are encoded: Form (RFC 6749) or JSON (e.g. monday.com).
	// +kubebuilder:validation:Enum=Form;JSON
	// +kubebuilder:default=Form
	// +optional
	TokenRequestFormat string `json:"tokenRequestFormat,omitempty"`
}

type ConnectionStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Token URL",type=string,JSONPath=`.spec.tokenURL`

// Connection is a SaaS app whose per-user grants Ambit holds for MCP servers.
type Connection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ConnectionSpec   `json:"spec"`
	Status            ConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Connection `json:"items"`
}

// BudgetSpec limits LLM use per day for one group from the identity provider: requests, estimated dollars
// (from Model prices), or both. Limits are shared across every gateway replica. A member of several
// budgeted groups is counted against each of them.
// +kubebuilder:validation:XValidation:rule="has(self.groupRequestsPerDay) || has(self.userRequestsPerDay) || has(self.groupDollarsPerDay) || has(self.userDollarsPerDay)",message="set at least one limit"
// +kubebuilder:validation:XValidation:rule="!has(self.groupDollarsPerDay) || double(self.groupDollarsPerDay) > 0.0",message="groupDollarsPerDay must be greater than 0"
// +kubebuilder:validation:XValidation:rule="!has(self.userDollarsPerDay) || double(self.userDollarsPerDay) > 0.0",message="userDollarsPerDay must be greater than 0"
// +kubebuilder:validation:XValidation:rule="!has(self.groupDollarsPerDay) || double(self.groupDollarsPerDay) <= 42949.67",message="groupDollarsPerDay must be at most 42949.67"
// +kubebuilder:validation:XValidation:rule="!has(self.userDollarsPerDay) || double(self.userDollarsPerDay) <= 42949.67",message="userDollarsPerDay must be at most 42949.67"
type BudgetSpec struct {
	// Group from the identity provider's "groups" claim.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._@/-]*$`
	Group string `json:"group"`
	// GroupRequestsPerDay is shared by everyone in the group (UTC days).
	// +kubebuilder:validation:Minimum=1
	// +optional
	GroupRequestsPerDay *int64 `json:"groupRequestsPerDay,omitempty"`
	// UserRequestsPerDay applies to each member separately (UTC days).
	// +kubebuilder:validation:Minimum=1
	// +optional
	UserRequestsPerDay *int64 `json:"userRequestsPerDay,omitempty"`
	// GroupDollarsPerDay is shared by everyone in the group (UTC days), estimated from Model prices. Answers
	// already started when the cap is passed still finish and are charged.
	// +kubebuilder:validation:Pattern=`^[0-9]{1,5}(\.[0-9]{1,5})?$`
	// +optional
	GroupDollarsPerDay string `json:"groupDollarsPerDay,omitempty"`
	// UserDollarsPerDay applies to each member separately.
	// +kubebuilder:validation:Pattern=`^[0-9]{1,5}(\.[0-9]{1,5})?$`
	// +optional
	UserDollarsPerDay string `json:"userDollarsPerDay,omitempty"`
}

type BudgetStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.group`
// +kubebuilder:printcolumn:name="Group/day",type=integer,JSONPath=`.spec.groupRequestsPerDay`
// +kubebuilder:printcolumn:name="User/day",type=integer,JSONPath=`.spec.userRequestsPerDay`
// +kubebuilder:printcolumn:name="Group $/day",type=string,JSONPath=`.spec.groupDollarsPerDay`
// +kubebuilder:printcolumn:name="User $/day",type=string,JSONPath=`.spec.userDollarsPerDay`

// Budget caps a group's LLM requests per day. Over the limit the gateway answers 429.
type Budget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BudgetSpec   `json:"spec"`
	Status            BudgetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type BudgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Budget `json:"items"`
}

// ServerRequestSpec asks for a new MCP server. It cannot change after it is made, so what was
// approved is exactly what was asked for.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a server request cannot be changed; make a new one"
type ServerRequestSpec struct {
	// ServerName is the name the Server gets when the request is approved.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	ServerName string `json:"serverName"`
	// Server is what will run, including the image digest.
	Server ServerSpec `json:"server"`
	// Reason the requester gives, shown to the approver.
	// +kubebuilder:validation:MaxLength=2000
	// +optional
	Reason string `json:"reason,omitempty"`
	// RequestedBy is the requester's email, set by the console from their sign-in.
	// +kubebuilder:validation:MinLength=1
	RequestedBy string `json:"requestedBy"`
}

// ServerRequestStatus records the decision. Once made, it cannot change.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.decision) || (has(self.decision) && self.decision == oldSelf.decision)",message="a decision cannot be changed"
type ServerRequestStatus struct {
	// Empty while the request waits for an approver.
	// +kubebuilder:validation:Enum=Approved;Denied
	// +optional
	Decision string `json:"decision,omitempty"`
	// +optional
	DecidedBy string `json:"decidedBy,omitempty"`
	// +optional
	DecidedAt *metav1.Time `json:"decidedAt,omitempty"`
	// +kubebuilder:validation:MaxLength=2000
	// +optional
	Comment string `json:"comment,omitempty"`
	// PullRequest is set in GitOps mode: approval opens it, and the Server appears when it is merged.
	// +optional
	PullRequest string `json:"pullRequest,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.serverName`
// +kubebuilder:printcolumn:name="Requested by",type=string,JSONPath=`.spec.requestedBy`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.status.decision`
// +kubebuilder:printcolumn:name="Decided by",type=string,JSONPath=`.status.decidedBy`

// ServerRequest asks an approver to add an MCP server. Approving it in the console creates the
// Server; the request stays as the record of who asked, who approved, and which image digest.
type ServerRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ServerRequestSpec   `json:"spec"`
	Status            ServerRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ServerRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServerRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Model{}, &ModelList{}, &AccessRule{}, &AccessRuleList{}, &Server{}, &ServerList{},
		&ServerRequest{}, &ServerRequestList{}, &Connection{}, &ConnectionList{}, &Budget{}, &BudgetList{}, &ModelRouter{}, &ModelRouterList{})
}
