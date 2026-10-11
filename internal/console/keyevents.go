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
	keyEventLabel  = "ambitproject.dev/key-event"
	keyEventAtAnn  = "ambitproject.dev/key-event-at"
	eventRetention = 400 * 24 * time.Hour
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
		Annotations: map[string]string{keyEventAtAnn: at.Format(time.RFC3339)}},
		Data: map[string]string{"actor": actor, "action": action, "keyId": keyID, "email": email, "detail": detail}}
	if err := s.Client.Create(ctx, cm); err != nil {
		slog.Error("key event not recorded", "action", action, "key", keyID, "err", err)
	}
	slog.Info("key event", "action", action, "key", keyID, "email", email, "actor", actor, "detail", detail)
}

func (s *KeyService) eventMaps(ctx context.Context) ([]corev1.ConfigMap, error) {
	var l corev1.ConfigMapList
	if err := s.Client.List(ctx, &l, client.InNamespace(s.Namespace), client.MatchingLabels{keyEventLabel: "true"}); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// Events returns the recorded key events, newest first.
func (s *KeyService) Events(ctx context.Context) ([]KeyEvent, error) {
	cms, err := s.eventMaps(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(cms, func(a, b corev1.ConfigMap) int { return strings.Compare(b.Name, a.Name) }) // name encodes time and order
	out := make([]KeyEvent, 0, len(cms))
	for _, cm := range cms {
		at, _ := time.Parse(time.RFC3339, cm.Annotations[keyEventAtAnn])
		d := cm.Data
		out = append(out, KeyEvent{At: at, Actor: d["actor"], Action: d["action"], KeyID: d["keyId"], Email: d["email"], Detail: d["detail"]})
	}
	return out, nil
}

// cleanupEvents deletes events older than the retention period. Call it holding s.mu.
func (s *KeyService) cleanupEvents(ctx context.Context) error {
	cms, err := s.eventMaps(ctx)
	if err != nil {
		return err
	}
	cutoff := s.now().Add(-eventRetention)
	var errs []error
	for i := range cms {
		at, err := time.Parse(time.RFC3339, cms[i].Annotations[keyEventAtAnn])
		if err != nil || !at.Before(cutoff) {
			continue
		}
		if err := s.Client.Delete(ctx, &cms[i]); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
