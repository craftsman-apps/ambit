package console

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Budgets shows how much of each group's daily LLM budget is used (journey A3), read from the counters
// envoy's ratelimit service keeps in Valkey. Key format (ratelimit's cache_key.go):
// "<domain>_group_<group>_<dayStartUnix>" and "<domain>_group_<group>_user_<email>_<dayStartUnix>".
type Budgets struct {
	Client    client.Client
	Namespace string
	Valkey    string // host:port
	Now       func() time.Time
}

const budgetDomain = "ambit-llm" // matches controller.RateLimitDomain

type UserUse struct {
	Email string `json:"email"`
	Used  int64  `json:"used"`
}

type BudgetView struct {
	Name          string    `json:"name"`
	Group         string    `json:"group"`
	GroupLimit    *int64    `json:"groupLimit"`
	UserLimit     *int64    `json:"userLimit"`
	GroupUsed     int64     `json:"groupUsed"`
	Users         []UserUse `json:"users"` // today's users, most used first
	Ready         string    `json:"ready"`
	Message       string    `json:"message"`
	ResetsAt      time.Time `json:"resetsAt"`
	CountsMissing bool      `json:"countsMissing,omitempty"` // Valkey unreachable
	GitManaged    bool      `json:"gitManaged"`
}

func (b *Budgets) register(mux *http.ServeMux, admin func(func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc) {
	mux.HandleFunc("GET /api/budgets", admin(func(w http.ResponseWriter, r *http.Request, _ Session) {
		out, err := b.views(r.Context())
		if err != nil {
			serverErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
}

func (b *Budgets) views(ctx context.Context) ([]BudgetView, error) {
	var l v1.BudgetList
	if err := b.Client.List(ctx, &l, client.InNamespace(b.Namespace)); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if b.Now != nil {
		now = b.Now().UTC()
	}
	day := now.Truncate(24 * time.Hour)
	suffix := "_" + strconv.FormatInt(day.Unix(), 10)
	out := []BudgetView{}
	for _, bu := range l.Items {
		v := BudgetView{Name: bu.Name, Group: bu.Spec.Group, GroupLimit: bu.Spec.GroupRequestsPerDay, UserLimit: bu.Spec.UserRequestsPerDay,
			Users: []UserUse{}, Ready: "Unknown", ResetsAt: day.Add(24 * time.Hour), GitManaged: gitManaged(bu.Annotations)}
		if c := meta.FindStatusCondition(bu.Status.Conditions, "Ready"); c != nil {
			v.Ready, v.Message = string(c.Status), c.Message
		}
		groupKey := budgetDomain + "_group_" + bu.Spec.Group
		counts, err := valkeyCounts(ctx, b.Valkey, groupKey+"_*"+suffix)
		if err != nil {
			v.CountsMissing = true
			out = append(out, v)
			continue
		}
		for k, n := range counts {
			if strings.HasPrefix(k, groupKey+"_user_") {
				v.Users = append(v.Users, UserUse{Email: strings.TrimSuffix(strings.TrimPrefix(k, groupKey+"_user_"), suffix), Used: n})
			}
		}
		if n, err := valkeyGet(ctx, b.Valkey, groupKey+suffix); err == nil { // the group key has no "_user_" part
			v.GroupUsed = n
		}
		sort.Slice(v.Users, func(i, j int) bool {
			if v.Users[i].Used != v.Users[j].Used {
				return v.Users[i].Used > v.Users[j].Used
			}
			return v.Users[i].Email < v.Users[j].Email
		})
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out, nil
}

// --- a minimal RESP client: GET and SCAN only, which is all the budget view needs ---

func respDo(ctx context.Context, addr string, args ...string) (any, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&sb, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(conn, sb.String()); err != nil {
		return nil, err
	}
	return respRead(bufio.NewReader(conn))
}

func respRead(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(line, "\r\n")
	if line == "" {
		return nil, errors.New("resp: empty line")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, errors.New("valkey: " + line[1:])
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err // nil bulk string: key missing
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		arr := make([]any, 0, max(n, 0))
		for range n {
			v, err := respRead(r)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	}
	return nil, fmt.Errorf("resp: unexpected %q", line)
}

func valkeyGet(ctx context.Context, addr, key string) (int64, error) {
	v, err := respDo(ctx, addr, "GET", key)
	if err != nil || v == nil {
		return 0, err
	}
	return strconv.ParseInt(fmt.Sprint(v), 10, 64)
}

// valkeyCounts returns every counter matching pattern. ponytail: SCAN plus one GET per key; fine for
// hundreds of users per group, use MGET or a Lua script beyond.
func valkeyCounts(ctx context.Context, addr, pattern string) (map[string]int64, error) {
	out := map[string]int64{}
	cursor := "0"
	for {
		v, err := respDo(ctx, addr, "SCAN", cursor, "MATCH", pattern, "COUNT", "500")
		if err != nil {
			return nil, err
		}
		arr, ok := v.([]any)
		if !ok || len(arr) != 2 {
			return nil, errors.New("valkey: unexpected SCAN reply")
		}
		keys, _ := arr[1].([]any)
		for _, k := range keys {
			n, err := valkeyGet(ctx, addr, fmt.Sprint(k))
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(k)] = n
		}
		if cursor = fmt.Sprint(arr[0]); cursor == "0" {
			return out, nil
		}
	}
}

// BudgetInput is the Budgets form. Name is ignored on update (the path names the budget).
type BudgetInput struct {
	Name                string `json:"name"`
	Group               string `json:"group"`
	GroupRequestsPerDay *int64 `json:"groupRequestsPerDay"`
	UserRequestsPerDay  *int64 `json:"userRequestsPerDay"`
}

// checkBudget validates the form; self is the budget being edited ("" when creating). A second
// budget for the same group would be refused by the controller, so it is refused here first.
func (c *Catalog) checkBudget(r *http.Request, in BudgetInput, self string) (int, error) {
	switch {
	case !groupRE.MatchString(in.Group):
		return http.StatusBadRequest, fmt.Errorf("group %q may only contain letters, digits and . _ @ / -", in.Group)
	case in.GroupRequestsPerDay == nil && in.UserRequestsPerDay == nil:
		return http.StatusBadRequest, errors.New("set a limit for the whole group, for each person, or both")
	case (in.GroupRequestsPerDay != nil && *in.GroupRequestsPerDay < 1) || (in.UserRequestsPerDay != nil && *in.UserRequestsPerDay < 1):
		return http.StatusBadRequest, errors.New("a limit must be at least 1 request per day")
	}
	var l v1.BudgetList
	if err := c.Client.List(r.Context(), &l, client.InNamespace(c.Namespace)); err != nil {
		return http.StatusInternalServerError, err
	}
	for _, b := range l.Items {
		if b.Spec.Group == in.Group && b.Name != self {
			return http.StatusConflict, fmt.Errorf("group %s already has a budget (%s); change that one", in.Group, b.Name)
		}
	}
	return 0, nil
}

func (c *Catalog) createBudget(w http.ResponseWriter, r *http.Request, s Session) {
	var in BudgetInput
	if !decodeInto(w, r, &in) {
		return
	}
	if !nameRE.MatchString(in.Name) {
		writeErr(w, http.StatusBadRequest, "name must be lowercase letters, digits and dashes, starting and ending with a letter or digit")
		return
	}
	if code, err := c.checkBudget(r, in, ""); err != nil {
		if code == http.StatusInternalServerError {
			serverErr(w, err)
		} else {
			writeErr(w, code, err.Error())
		}
		return
	}
	b := &v1.Budget{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: c.Namespace, Annotations: map[string]string{createdByKey: s.Email}},
		Spec: v1.BudgetSpec{Group: in.Group, GroupRequestsPerDay: in.GroupRequestsPerDay, UserRequestsPerDay: in.UserRequestsPerDay}}
	if c.GitOps != nil {
		c.propose(w, r, s, "create", "Budget", "budgets", in.Name, b.Spec, b.Annotations)
		return
	}
	if err := c.Client.Create(r.Context(), b); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeErr(w, http.StatusConflict, "a budget named "+in.Name+" already exists")
			return
		}
		apiErr(w, err)
		return
	}
	slog.Info("budget created", "budget", in.Name, "group", in.Group, "by", s.Email)
	writeJSON(w, http.StatusCreated, map[string]string{"name": in.Name})
}

func (c *Catalog) editableBudget(w http.ResponseWriter, r *http.Request) (*v1.Budget, bool) {
	var b v1.Budget
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("name")}, &b); err != nil {
		if apierrors.IsNotFound(err) {
			writeErr(w, http.StatusNotFound, "no budget named "+r.PathValue("name"))
		} else {
			serverErr(w, err)
		}
		return nil, false
	}
	if c.GitOps == nil && gitManaged(b.Annotations) {
		writeErr(w, http.StatusConflict, "this budget is managed in Git; change it with a pull request")
		return nil, false
	}
	return &b, true
}

func (c *Catalog) updateBudget(w http.ResponseWriter, r *http.Request, s Session) {
	var in BudgetInput
	if !decodeInto(w, r, &in) {
		return
	}
	b, ok := c.editableBudget(w, r)
	if !ok {
		return
	}
	if code, err := c.checkBudget(r, in, b.Name); err != nil {
		if code == http.StatusInternalServerError {
			serverErr(w, err)
		} else {
			writeErr(w, code, err.Error())
		}
		return
	}
	b.Spec = v1.BudgetSpec{Group: in.Group, GroupRequestsPerDay: in.GroupRequestsPerDay, UserRequestsPerDay: in.UserRequestsPerDay}
	if c.GitOps != nil {
		c.propose(w, r, s, "update", "Budget", "budgets", b.Name, b.Spec, consoleAnnotations(b.Annotations))
		return
	}
	if b.Annotations == nil {
		b.Annotations = map[string]string{}
	}
	b.Annotations[updatedByKey] = s.Email
	if err := c.Client.Update(r.Context(), b); err != nil {
		apiErr(w, err)
		return
	}
	slog.Info("budget updated", "budget", b.Name, "group", in.Group, "by", s.Email)
	writeJSON(w, http.StatusOK, map[string]string{"name": b.Name})
}

func (c *Catalog) deleteBudget(w http.ResponseWriter, r *http.Request, s Session) {
	b, ok := c.editableBudget(w, r)
	if !ok {
		return
	}
	if c.GitOps != nil {
		c.propose(w, r, s, "delete", "Budget", "budgets", b.Name, nil, nil)
		return
	}
	if err := c.Client.Delete(r.Context(), b); err != nil && !apierrors.IsNotFound(err) {
		apiErr(w, err)
		return
	}
	slog.Info("budget deleted", "budget", b.Name, "by", s.Email)
	w.WriteHeader(http.StatusNoContent)
}
