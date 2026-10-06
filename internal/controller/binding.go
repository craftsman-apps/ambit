package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DestinationHash names a server URL in the broker path (Task 6). A gateway still running an older
// configuration carries the old URL's hash, so the broker can refuse it a token.
func DestinationHash(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])[:16]
}

// BindServers drops every Server that names a Connection bound to another URL: a user's token for an app
// must never go anywhere but that app's server.
func BindServers(servers []v1.Server, conns []v1.Connection) ([]v1.Server, map[string]string) {
	bound := map[string]string{}
	for _, c := range conns {
		bound[c.Name] = c.Spec.ServerURL
	}
	var kept []v1.Server
	refused := map[string]string{}
	for _, s := range servers {
		if want := bound[s.Spec.Connection]; s.Spec.Connection != "" && want != "" && s.Spec.URL != want {
			refused[s.Name] = "connection " + s.Spec.Connection + " is bound to " + want + "; this server's url is " + s.Spec.URL
			continue
		}
		kept = append(kept, s)
	}
	return kept, refused
}

// BrokerPath is the extAuth path for one per-user server: destination-checked for a bound Connection.
func BrokerPath(s v1.Server, bound bool) string {
	if !bound {
		return "/v1/connection/" + s.Spec.Connection
	}
	return "/v1/connection/" + url.PathEscape(s.Spec.Connection) + "/server/" + url.PathEscape(s.Name) + "/" + DestinationHash(s.Spec.URL)
}

// SetBrokerPaths gives every per-user server whose Connection has serverURL the destination-checked path.
func SetBrokerPaths(objs []*unstructured.Unstructured, servers []v1.Server, conns []v1.Connection) {
	bound := map[string]bool{}
	for _, c := range conns {
		bound[c.Name] = c.Spec.ServerURL != ""
	}
	byPolicy := map[string]v1.Server{}
	for _, s := range servers {
		byPolicy["ambit-conn-"+s.Name] = s
	}
	for _, o := range objs {
		s, ok := byPolicy[o.GetName()]
		if !ok || o.GetKind() != "AgentgatewayPolicy" || s.Spec.Connection == "" {
			continue
		}
		_ = unstructured.SetNestedField(o.Object, strconv.Quote(BrokerPath(s, bound[s.Spec.Connection])), "spec", "backend", "extAuth", "http", "path")
	}
}
