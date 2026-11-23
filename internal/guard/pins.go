package guard

import (
	"context"
	"log/slog"

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
	if server == "" {
		return nil, nil, false
	}
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: server}, &s); err != nil {
		// Unknown pins are not "no pins": nothing is approved, so every tool is hidden and every call refused (issue 300).
		slog.Error("guard: cannot read the Server; its tools are refused", "server", server, "err", err)
		return map[string]string{}, map[string]string{}, true
	}
	if len(s.Status.ApprovedTools) == 0 {
		if s.Spec.Connection != "" {
			// A per-person server whose tools no admin has read: usable without pinning (issue 300, option C). The
			// controller's ToolsPinned=False (NotPinned) condition and the Servers page show it.
			return nil, nil, false
		}
		// A shared server whose tools have not been read yet: nothing is approved, so its tools are hidden and refused
		// until the controller reads (and so pins) them.
		return map[string]string{}, map[string]string{}, true
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
