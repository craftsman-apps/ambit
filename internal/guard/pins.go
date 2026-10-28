package guard

import (
	"context"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubePins reads pins from Server objects (a cached client, so each check costs no API call).
type KubePins struct {
	Client    client.Reader
	Namespace string
}

func (k KubePins) Pins(ctx context.Context, server string) (approved, current map[string]string, pinned bool) {
	var s v1.Server
	if server == "" || k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: server}, &s) != nil {
		return nil, nil, false
	}
	if len(s.Status.ApprovedTools) == 0 {
		return nil, nil, false
	}
	approved, current = map[string]string{}, map[string]string{}
	for _, p := range s.Status.ApprovedTools {
		approved[p.Name] = p.Fingerprint
	}
	for _, t := range s.Status.Tools {
		current[t.Name] = t.Fingerprint
	}
	return approved, current, true
}

// KubeCompression reads the result compression setting from Server objects (a cached client, no API call).
type KubeCompression struct {
	Client    client.Reader
	Namespace string
}

func (k KubeCompression) Compression(ctx context.Context, server string) CompressionConfig {
	var s v1.Server
	if server == "" || k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: server}, &s) != nil || s.Spec.ResultCompression == nil {
		return CompressionConfig{Mode: ModeOff}
	}
	c := s.Spec.ResultCompression
	if c.MinBytes == 0 {
		return CompressionConfig{Mode: c.Mode, MinBytes: DefaultMinBytes} // the API server defaults it; this guards an old object
	}
	return CompressionConfig{Mode: c.Mode, MinBytes: int(c.MinBytes)}
}
