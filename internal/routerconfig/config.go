// Package routerconfig is the configuration the controller publishes for ambit-router (ConfigMap
// ambit-router-config, key config.json) and the hash that ties a label to the virtual-model lines that read it.
package routerconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	ConfigMapName = "ambit-router-config"
	ConfigKey     = "config.json"
	// SecretName holds the classifier Models' keys, one entry per Model object name.
	SecretName = "ambit-router-credentials"
)

type Config struct {
	// Hash is HashOf(Routers); the gateway passes it per request, and ambit-router labels only when they agree.
	Hash    string   `json:"hash"`
	Routers []Router `json:"routers"`
}

type Router struct {
	Name          string     `json:"name"` // ModelRouter object name
	Wire          string     `json:"wire"` // model name clients send
	Type          string     `json:"type"`
	Categories    []Category `json:"categories"`
	Groups        []string   `json:"groups,omitempty"`
	AllSignedIn   bool       `json:"allSignedIn,omitempty"`
	LLM           *LLM       `json:"llm,omitempty"`
	TimeoutMillis int64      `json:"timeoutMillis"`
	MaxBytes      int64      `json:"maxBytes"`
	RatePerMinute int32      `json:"ratePerMinute"`
}

type Category struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type LLM struct {
	Endpoint      string `json:"endpoint"`      // base URL, without /chat/completions
	Model         string `json:"model"`         // provider model id (Model.spec.model)
	CredentialKey string `json:"credentialKey"` // entry in ambit-router-credentials: the Model's object name
}

// HashOf is the hex SHA-256 of the routers' JSON; the controller sorts them by name.
func HashOf(routers []Router) string {
	b, _ := json.Marshal(routers)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c Config) Find(wire string) (Router, bool) {
	for _, r := range c.Routers {
		if r.Wire == wire {
			return r, true
		}
	}
	return Router{}, false
}
