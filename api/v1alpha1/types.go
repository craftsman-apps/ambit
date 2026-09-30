package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ModelSpec is one entry in the model catalog: a model clients may request through the gateway.
type ModelSpec struct {
	// Provider serving the model. Values follow agentgateway's AgentgatewayModel providers.
	// +kubebuilder:validation:Enum=Anthropic;Azure;Bedrock;Gemini;OpenAI;Openrouter;VertexAI
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

// ServerSpec is an MCP server in the Ambit catalog. ToolHive runs it in a pod; clients reach its
// tools only through the gateway, as "<server name>_<tool>".
type ServerSpec struct {
	// Image pinned by digest, e.g. ghcr.io/org/server@sha256:<64 hex>. A tag can later point to
	// different code; a digest cannot, so what was reviewed is what runs.
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`
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
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type Tool struct {
	Name string `json:"name"`
	// +optional
	Description string `json:"description,omitempty"`
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
		&ServerRequest{}, &ServerRequestList{})
}
