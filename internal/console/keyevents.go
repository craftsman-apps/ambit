package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	keyEventLabel     = "ambitproject.dev/key-event"
	keyEventAtAnn     = "ambitproject.dev/key-event-at"
	keyEventActionAnn = "ambitproject.dev/key-event-action"
	eventRetention    = 400 * 24 * time.Hour
)

var (
	eventPage  int64 = 500 // key events are listed this many at a time
	historyMax       = 500 // the History shows at most this many newest events
)

type KeyEvent struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	KeyID  string    `json:"keyId"`
	Email  string    `json:"email"`
	Detail string    `json:"detail"`
}

// event records one key change as its own ConfigMap, never holding the key or its hash. Failure to record is
// logged, not fatal: the change already happened. Call it holding s.mu.
func (s *KeyService) event(ctx context.Context, actor, action, keyID, email, detail string) {
	at := s.now()
	s.seq++
	suffix, _ := newKeyID()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: fmt.Sprintf("ambit-key-event-%010d-%06d-%s", at.Unix(), s.seq%1000000, suffix), Namespace: s.Namespace,
		Labels:      map[string]string{keyEventLabel: "true"},
		Annotations: map[string]string{keyEventAtAnn: at.Format(time.RFC3339), keyEventActionAnn: action}},
		Data: map[string]string{"actor": actor, "action": action, "keyId": keyID, "email": email, "detail": detail}}
	if err := s.Client.Create(ctx, cm); err != nil {
		slog.Error("key event not recorded", "action", action, "key", keyID, "err", err)
	}
	slog.Info("key event", "action", action, "key", keyID, "email", email, "actor", actor, "detail", detail)
}

// eventListOpts selects key events in the namespace, one page at a time.
func (s *KeyService) eventListOpts(cont string) []client.ListOption {
	return []client.ListOption{client.InNamespace(s.Namespace), client.MatchingLabels{keyEventLabel: "true"},
		client.Limit(eventPage), client.Continue(cont)}
}

// eachEventMeta calls fn with every key event's metadata (no data), a page at a time.
func (s *KeyService) eachEventMeta(ctx context.Context, fn func(m *metav1.PartialObjectMetadata)) error {
	for cont := ""; ; {
		var l metav1.PartialObjectMetadataList
		l.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMapList"))
		if err := s.Client.List(ctx, &l, s.eventListOpts(cont)...); err != nil {
			return err
		}
		for i := range l.Items {
			fn(&l.Items[i])
		}
		if cont = l.Continue; cont == "" {
			return nil
		}
	}
}

// Events returns the newest recorded key events (at most historyMax), newest first. Pages are read one at a
// time and only the newest historyMax are kept, so memory stays bounded however many events exist.
func (s *KeyService) Events(ctx context.Context) ([]KeyEvent, error) {
	var keep []corev1.ConfigMap
	newestFirst := func(a, b corev1.ConfigMap) int { return strings.Compare(b.Name, a.Name) } // name encodes time and order
	for cont := ""; ; {
		var l corev1.ConfigMapList
		if err := s.Client.List(ctx, &l, s.eventListOpts(cont)...); err != nil {
			return nil, err
		}
		keep = append(keep, l.Items...)
		slices.SortFunc(keep, newestFirst)
		keep = keep[:min(len(keep), historyMax)]
		if cont = l.Continue; cont == "" {
			break
		}
	}
	out := make([]KeyEvent, 0, len(keep))
	for _, cm := range keep {
		at, _ := time.Parse(time.RFC3339, cm.Annotations[keyEventAtAnn])
		d := cm.Data
		out = append(out, KeyEvent{At: at, Actor: d["actor"], Action: d["action"], KeyID: d["keyId"], Email: d["email"], Detail: d["detail"]})
	}
	return out, nil
}

// cleanupEvents deletes events older than the retention period. Call it holding s.mu.
func (s *KeyService) cleanupEvents(ctx context.Context) error {
	cutoff := s.now().Add(-eventRetention)
	var old []string
	if err := s.eachEventMeta(ctx, func(m *metav1.PartialObjectMetadata) {
		if at, err := time.Parse(time.RFC3339, m.Annotations[keyEventAtAnn]); err == nil && at.Before(cutoff) {
			old = append(old, m.Name)
		}
	}); err != nil {
		return err
	}
	var errs []error
	for _, name := range old {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
		if err := s.Client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
