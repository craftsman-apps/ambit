package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ModelRouterSpec is a model name clients may ask for (for example "auto") whose answer comes from one of
// several Models, chosen per request by ordered routes, within each user's access rules.
type ModelRouterSpec struct {
	// Model is the name clients send. Defaults to metadata.name. It must not be served by a Model.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`
	// +kubebuilder:validation:MaxLength=200
	// +optional
	Model string `json:"model,omitempty"`
	// Default is the Model (object name) that answers when no route matches.
	// +kubebuilder:validation:MinLength=1
	Default string `json:"default"`
	// +optional
	Classifier RouterClassifier `json:"classifier,omitempty"`
	// Routes are tried in order; the first whose conditions all hold and whose Model the user may use wins.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Routes []RouterRoute `json:"routes,omitempty"`
}

type RouterClassifier struct {
	// Type None ignores every route (always the default); Rules uses keyword and length routes.
	// +kubebuilder:validation:Enum=None;Rules
	// +kubebuilder:default=None
	// +optional
	Type string `json:"type,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.category) || has(self.keywords) || has(self.minBytes) || has(self.maxBytes)",message="a route needs a category, keywords, minBytes or maxBytes"
type RouterRoute struct {
	// Model (object name) that answers when this route matches.
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// Category is a label from a classifier (types VSR and LLM, added later). With type Rules or None a route
	// with a category is ignored (condition CategoryNeedsClassifier).
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,62}$`
	// +optional
	Category string `json:"category,omitempty"`
	// Keywords: any one found in the last user message matches (ASCII case-insensitive).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^[ -~]{1,100}$`
	// +optional
	Keywords []string `json:"keywords,omitempty"`
	// MinBytes: the last user message is at least this long, in UTF-8 bytes.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinBytes *int64 `json:"minBytes,omitempty"`
	// MaxBytes: the last user message is at most this long, in UTF-8 bytes.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxBytes *int64 `json:"maxBytes,omitempty"`
}

type ModelRouterStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="Default",type=string,JSONPath=`.spec.default`
// +kubebuilder:printcolumn:name="Classifier",type=string,JSONPath=`.spec.classifier.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// ModelRouter lets clients ask for one model name and get the right Model per request.
type ModelRouter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelRouterSpec   `json:"spec"`
	Status            ModelRouterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ModelRouterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelRouter `json:"items"`
}

// WireName is the model name clients send for this router.
func (r *ModelRouter) WireName() string {
	if r.Spec.Model != "" {
		return r.Spec.Model
	}
	return r.Name
}
