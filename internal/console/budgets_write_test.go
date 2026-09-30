package console

import (
	"net/http"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Journey A3 from the console: an admin sets, changes and removes a group's daily budget.
func TestBudgetWrites(t *testing.T) {
	h, c, auth := catalogFixture(t)
	admin := as(auth, RoleAdmin)
	if r := do(t, h, "POST", "/api/budgets", as(auth, RoleViewer), BudgetInput{Name: "eng", Group: "eng", UserRequestsPerDay: ptr.To[int64](10)}); r.Code != http.StatusForbidden {
		t.Fatalf("viewer: %d %s", r.Code, r.Body)
	}
	if r := do(t, h, "POST", "/api/budgets", admin, BudgetInput{Name: "eng", Group: "eng", UserRequestsPerDay: ptr.To[int64](10)}); r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	var b v1.Budget
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "eng"}, &b); err != nil || *b.Spec.UserRequestsPerDay != 10 ||
		b.Spec.GroupRequestsPerDay != nil || b.Annotations[createdByKey] != "admin@acme.test" {
		t.Fatalf("created: %+v %v", b, err)
	}
	for name, in := range map[string]BudgetInput{
		"second budget for a group": {Name: "eng-2", Group: "eng", GroupRequestsPerDay: ptr.To[int64](5)},
		"no limit":                  {Name: "ops", Group: "ops"},
		"zero limit":                {Name: "ops", Group: "ops", UserRequestsPerDay: ptr.To[int64](0)},
		"bad group":                 {Name: "ops", Group: "ops team", UserRequestsPerDay: ptr.To[int64](5)},
		"bad name":                  {Name: "Ops!", Group: "ops", UserRequestsPerDay: ptr.To[int64](5)},
	} {
		if r := do(t, h, "POST", "/api/budgets", admin, in); r.Code != http.StatusBadRequest && r.Code != http.StatusConflict {
			t.Errorf("%s accepted: %d %s", name, r.Code, r.Body)
		}
	}
	if r := do(t, h, "PUT", "/api/budgets/eng", admin, BudgetInput{Group: "eng", GroupRequestsPerDay: ptr.To[int64](100)}); r.Code != http.StatusOK {
		t.Fatalf("update: %d %s", r.Code, r.Body)
	}
	b = v1.Budget{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "eng"}, &b); err != nil || *b.Spec.GroupRequestsPerDay != 100 || b.Spec.UserRequestsPerDay != nil {
		t.Fatalf("updated: %+v %v", b.Spec, err)
	}
	if r := do(t, h, "DELETE", "/api/budgets/eng", admin, nil); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.Code, r.Body)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "eng"}, &b); err == nil {
		t.Fatal("budget still there")
	}
	// A budget kept in Git is read-only in Direct mode, like access rules.
	git := &v1.Budget{ObjectMeta: metav1.ObjectMeta{Name: "ml", Namespace: testNS, Annotations: map[string]string{"argocd.argoproj.io/tracking-id": "x"}},
		Spec: v1.BudgetSpec{Group: "ml-research", UserRequestsPerDay: ptr.To[int64](5)}}
	if err := c.Create(t.Context(), git); err != nil {
		t.Fatal(err)
	}
	if r := do(t, h, "PUT", "/api/budgets/ml", admin, BudgetInput{Group: "ml-research", UserRequestsPerDay: ptr.To[int64](9)}); r.Code != http.StatusConflict {
		t.Fatalf("git-managed: %d %s", r.Code, r.Body)
	}
}
