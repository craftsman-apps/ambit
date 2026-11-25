package controller

import (
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/apprecord"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// AppRemovals writes an app-approval record (action "removed") when a Server object is deleted, whoever deletes it:
// the console, kubectl, or Argo CD pruning a Server that left the Git repository (GitOps mode). The record has the
// server, its request, who requested it and its image or URL, as the other app records have. The actor is known only
// when the console deleted the Server (it names the person in annotations just before); otherwise it is empty, and
// the Git history or the Kubernetes audit log names who did it. Sending is best effort, as for the access snapshots.
type AppRemovals struct {
	Send func(*logspb.LogRecord) // otlplog.Exporter.Send: never blocks
	Now  func() time.Time        // nil: time.Now
}

// Removed records the deletion of srv, the last state the controller's cache held.
func (a *AppRemovals) Removed(srv *v1.Server) {
	if a == nil {
		return
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	ann := srv.Annotations
	a.Send(apprecord.Record(apprecord.Event{Time: now().UTC(), Action: apprecord.Removed,
		Actor: ann[apprecord.AnnRemovedBy], Role: ann[apprecord.AnnRemovedByRole], Server: srv.Name,
		Request: ann[apprecord.AnnRequest], RequestedBy: ann[apprecord.AnnRequestedBy], Image: srv.Spec.Image, URL: srv.Spec.URL}))
}
