package controller

import (
	"crypto/sha256"
	"encoding/hex"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
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
