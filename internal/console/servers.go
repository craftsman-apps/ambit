package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Annotations on a Server created by approving a ServerRequest.
const (
	requestedByKey = "ambitproject.dev/requested-by"
	approvedByKey  = "ambitproject.dev/approved-by"
	requestKey     = "ambitproject.dev/server-request"
)

var (
	serverNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`) // 40 characters, as the CRD
	digestRE     = regexp.MustCompile(`^[^@\s]+@sha256:[a-f0-9]{64}$`)        // same pattern as the CRD
)

type ServerView struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Image       string    `json:"image"`
	Transport   string    `json:"transport"`
	Ready       string    `json:"ready"`
	Message     string    `json:"message"`
	Tools       []v1.Tool `json:"tools"`
	RequestedBy string    `json:"requestedBy"`
	ApprovedBy  string    `json:"approvedBy"`
	GitManaged  bool      `json:"gitManaged"`
	// Rules that name this server.
	Rules []string `json:"rules"`
}

type RequestView struct {
	Name        string     `json:"name"`
	ServerName  string     `json:"serverName"`
	DisplayName string     `json:"displayName"`
	Image       string     `json:"image"`
	Transport   string     `json:"transport"`
	Reason      string     `json:"reason"`
	RequestedBy string     `json:"requestedBy"`
	RequestedAt time.Time  `json:"requestedAt"`
	Decision    string     `json:"decision"`
	DecidedBy   string     `json:"decidedBy"`
	DecidedAt   *time.Time `json:"decidedAt"`
	Comment     string     `json:"comment"`
}

type RequestInput struct {
	ServerName  string `json:"serverName"`
	DisplayName string `json:"displayName"`
	Image       string `json:"image"`
	Transport   string `json:"transport"`
	Reason      string `json:"reason"`
}

type DecisionInput struct {
	Comment string `json:"comment"`
}

// MyAccess is what the signed-in user may use, so a denied user can see why (journey U3).
type MyAccess struct {
	Groups  []string       `json:"groups"`
	Models  []string       `json:"models"`
	Servers []ServerAccess `json:"servers"`
}

type ServerAccess struct {
	Server   string   `json:"server"`
	AllTools bool     `json:"allTools"`
	Tools    []string `json:"tools"`
	Rules    []string `json:"rules"`
}

func (c *Catalog) servers(r *http.Request) ([]v1.Server, error) {
	var l v1.ServerList
	if err := c.Client.List(r.Context(), &l, client.InNamespace(c.Namespace)); err != nil {
		return nil, err
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	return l.Items, nil
}

func (c *Catalog) listServers(w http.ResponseWriter, r *http.Request, _ Session) {
	servers, err := c.servers(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	_, rules, err := c.load(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	out := []ServerView{}
	for _, s := range servers {
		v := ServerView{Name: s.Name, DisplayName: s.Spec.DisplayName, Image: s.Spec.Image, Transport: s.Spec.Transport,
			Ready: "Unknown", Tools: s.Status.Tools, RequestedBy: s.Annotations[requestedByKey], ApprovedBy: s.Annotations[approvedByKey],
			GitManaged: gitManaged(s.Annotations), Rules: []string{}}
		if v.Tools == nil {
			v.Tools = []v1.Tool{}
		}
		if cond := meta.FindStatusCondition(s.Status.Conditions, "Ready"); cond != nil {
			v.Ready, v.Message = string(cond.Status), cond.Message
		}
		for _, ar := range rules {
			if slices.ContainsFunc(ar.Spec.Servers, func(sa v1.ServerAccess) bool { return sa.Name == s.Name }) {
				v.Rules = append(v.Rules, ar.Name)
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func requestView(q v1.ServerRequest) RequestView {
	v := RequestView{Name: q.Name, ServerName: q.Spec.ServerName, DisplayName: q.Spec.Server.DisplayName, Image: q.Spec.Server.Image,
		Transport: q.Spec.Server.Transport, Reason: q.Spec.Reason, RequestedBy: q.Spec.RequestedBy, RequestedAt: q.CreationTimestamp.Time,
		Decision: q.Status.Decision, DecidedBy: q.Status.DecidedBy, Comment: q.Status.Comment}
	if q.Status.DecidedAt != nil {
		t := q.Status.DecidedAt.Time
		v.DecidedAt = &t
	}
	return v
}

func (c *Catalog) requests(r *http.Request) ([]v1.ServerRequest, error) {
	var l v1.ServerRequestList
	if err := c.Client.List(r.Context(), &l, client.InNamespace(c.Namespace)); err != nil {
		return nil, err
	}
	// Newest first: the approver's queue and the history read top-down.
	sort.Slice(l.Items, func(i, j int) bool {
		if !l.Items[i].CreationTimestamp.Equal(&l.Items[j].CreationTimestamp) {
			return l.Items[j].CreationTimestamp.Before(&l.Items[i].CreationTimestamp)
		}
		return l.Items[i].Name < l.Items[j].Name
	})
	return l.Items, nil
}

func (c *Catalog) listRequests(w http.ResponseWriter, r *http.Request, _ Session) {
	qs, err := c.requests(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	out := []RequestView{}
	for _, q := range qs {
		out = append(out, requestView(q))
	}
	writeJSON(w, http.StatusOK, out)
}

func decodeInto(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func (c *Catalog) validateRequest(r *http.Request, in RequestInput) error {
	switch {
	case !serverNameRE.MatchString(in.ServerName):
		return errors.New("server name must be lowercase letters, digits and dashes, at most 40 characters")
	case !digestRE.MatchString(in.Image):
		return errors.New("pin the image by digest, like ghcr.io/org/server@sha256:<64 hex characters>; a tag can later point to different code")
	case !slices.Contains([]string{"", "stdio", "streamable-http", "sse"}, in.Transport):
		return errors.New("transport must be stdio, streamable-http or sse")
	case len(in.Reason) > 2000 || len(in.DisplayName) > 200:
		return errors.New("reason or display name is too long")
	}
	var existing v1.Server
	err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: in.ServerName}, &existing)
	if err == nil {
		return fmt.Errorf("a server named %q already exists", in.ServerName)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	qs, err := c.requests(r)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(qs, func(q v1.ServerRequest) bool { return q.Spec.ServerName == in.ServerName && q.Status.Decision == "" }) {
		return fmt.Errorf("a request for %q is already waiting for an approver", in.ServerName)
	}
	return nil
}

// createRequest lets anyone signed in ask for a server; nothing runs until an approver approves.
func (c *Catalog) createRequest(w http.ResponseWriter, r *http.Request, s Session) {
	var in RequestInput
	if !decodeInto(w, r, &in) {
		return
	}
	if err := c.validateRequest(r, in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	transport := in.Transport
	if transport == "" {
		transport = "stdio"
	}
	q := &v1.ServerRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: in.ServerName + "-", Namespace: c.Namespace},
		Spec: v1.ServerRequestSpec{ServerName: in.ServerName, Reason: in.Reason, RequestedBy: s.Email,
			Server: v1.ServerSpec{Image: in.Image, Transport: transport, DisplayName: in.DisplayName}},
	}
	if err := c.Client.Create(r.Context(), q); err != nil {
		apiErr(w, err)
		return
	}
	slog.Info("server requested", "request", q.Name, "server", in.ServerName, "image", in.Image, "by", s.Email)
	writeJSON(w, http.StatusCreated, requestView(*q))
}

// pending loads a request that still waits for a decision by someone other than the requester.
func (c *Catalog) pending(w http.ResponseWriter, r *http.Request, s Session) (*v1.ServerRequest, bool) {
	var q v1.ServerRequest
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("name")}, &q); err != nil {
		if apierrors.IsNotFound(err) {
			writeErr(w, http.StatusNotFound, "no server request named "+r.PathValue("name"))
		} else {
			serverErr(w, err)
		}
		return nil, false
	}
	if q.Status.Decision != "" {
		writeErr(w, http.StatusConflict, fmt.Sprintf("this request was already %s by %s", lower(q.Status.Decision), q.Status.DecidedBy))
		return nil, false
	}
	// Separation of duties: nobody approves their own request, admins included.
	if q.Spec.RequestedBy == s.Email {
		writeErr(w, http.StatusForbidden, "you made this request, so another approver must decide on it")
		return nil, false
	}
	return &q, true
}

func lower(decision string) string {
	if decision == "Approved" {
		return "approved"
	}
	return "denied"
}

// decide records the decision on the request. The request's status can change only once (CRD rule),
// and the resourceVersion check makes a second, concurrent decision fail instead of overwrite.
func (c *Catalog) decide(w http.ResponseWriter, r *http.Request, s Session, q *v1.ServerRequest, decision, comment string) bool {
	now := metav1.Now()
	q.Status = v1.ServerRequestStatus{Decision: decision, DecidedBy: s.Email, DecidedAt: &now, Comment: comment}
	if err := c.Client.Status().Update(r.Context(), q); err != nil {
		if apierrors.IsConflict(err) {
			writeErr(w, http.StatusConflict, "someone else decided on this request first; reload")
			return false
		}
		apiErr(w, err)
		return false
	}
	return true
}

func (c *Catalog) approveRequest(w http.ResponseWriter, r *http.Request, s Session) {
	var in DecisionInput
	if !decodeInto(w, r, &in) {
		return
	}
	q, ok := c.pending(w, r, s)
	if !ok {
		return
	}
	srv := &v1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: q.Spec.ServerName, Namespace: c.Namespace, Annotations: map[string]string{
			requestedByKey: q.Spec.RequestedBy, approvedByKey: s.Email, requestKey: q.Name}},
		Spec: q.Spec.Server,
	}
	if err := c.Client.Create(r.Context(), srv); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			apiErr(w, err)
			return
		}
		// A retry after the status update failed finds its own Server; anything else is a real clash.
		var existing v1.Server
		if err := c.Client.Get(r.Context(), client.ObjectKeyFromObject(srv), &existing); err != nil || existing.Annotations[requestKey] != q.Name {
			writeErr(w, http.StatusConflict, fmt.Sprintf("a server named %q already exists; deny this request", q.Spec.ServerName))
			return
		}
	}
	if !c.decide(w, r, s, q, "Approved", in.Comment) {
		return
	}
	slog.Info("server request approved", "request", q.Name, "server", q.Spec.ServerName, "image", q.Spec.Server.Image,
		"requestedBy", q.Spec.RequestedBy, "approvedBy", s.Email)
	writeJSON(w, http.StatusOK, requestView(*q))
}

func (c *Catalog) denyRequest(w http.ResponseWriter, r *http.Request, s Session) {
	var in DecisionInput
	if !decodeInto(w, r, &in) {
		return
	}
	q, ok := c.pending(w, r, s)
	if !ok {
		return
	}
	if !c.decide(w, r, s, q, "Denied", in.Comment) {
		return
	}
	slog.Info("server request denied", "request", q.Name, "server", q.Spec.ServerName, "by", s.Email)
	writeJSON(w, http.StatusOK, requestView(*q))
}

// myAccess lists what the rules grant the signed-in user's groups. The groups come from the console
// sign-in; the gateway checks the groups in the client's own token, which normally match.
func (c *Catalog) myAccess(w http.ResponseWriter, r *http.Request, s Session) {
	models, rules, err := c.load(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	servers, err := c.servers(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	out := MyAccess{Groups: nonNil(s.Groups), Models: []string{}, Servers: []ServerAccess{}}
	bySrv := map[string]*ServerAccess{}
	for _, ar := range rules {
		if !ar.Spec.AllSignedIn && !slices.ContainsFunc(ar.Spec.Groups, func(g string) bool { return slices.Contains(s.Groups, g) }) {
			continue
		}
		for _, m := range ar.Spec.Models {
			if slices.ContainsFunc(models, func(x v1.Model) bool { return x.Name == m }) && !slices.Contains(out.Models, m) {
				out.Models = append(out.Models, m)
			}
		}
		for _, sa := range ar.Spec.Servers {
			if !slices.ContainsFunc(servers, func(x v1.Server) bool { return x.Name == sa.Name }) {
				continue
			}
			a := bySrv[sa.Name]
			if a == nil {
				a = &ServerAccess{Server: sa.Name, Tools: []string{}}
				bySrv[sa.Name] = a
			}
			a.Rules = append(a.Rules, ar.Name)
			a.AllTools = a.AllTools || len(sa.Tools) == 0
			for _, t := range sa.Tools {
				if !slices.Contains(a.Tools, t) {
					a.Tools = append(a.Tools, t)
				}
			}
		}
	}
	sort.Strings(out.Models)
	for _, a := range bySrv {
		sort.Strings(a.Tools)
		out.Servers = append(out.Servers, *a)
	}
	sort.Slice(out.Servers, func(i, j int) bool { return out.Servers[i].Server < out.Servers[j].Server })
	writeJSON(w, http.StatusOK, out)
}
