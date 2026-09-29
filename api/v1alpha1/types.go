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
	// The key itself never appears in this object, so the object is safe to keep in Git.
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

// AccessRuleSpec says who may use which models. Rules only grant access; nothing is allowed by default.
// +kubebuilder:validation:XValidation:rule="(has(self.groups) && size(self.groups) > 0) != (has(self.allSignedIn) && self.allSignedIn)",message="set either groups or allSignedIn: true, not both"
type AccessRuleSpec struct {
	// Groups from the identity provider's "groups" claim. A member of any listed group is allowed.
	// +optional
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9][A-Za-z0-9._@/-]*$`
	Groups []string `json:"groups,omitempty"`
	// AllSignedIn allows everyone with a valid token.
	// +optional
	AllSignedIn bool `json:"allSignedIn,omitempty"`
	// Models lists Model object names in this namespace.
	// +kubebuilder:validation:MinItems=1
	Models []string `json:"models"`
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
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// AccessRule grants groups (or everyone signed in) the use of models.
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

func init() {
	SchemeBuilder.Register(&Model{}, &ModelList{}, &AccessRule{}, &AccessRuleList{})
}
