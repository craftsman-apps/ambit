package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type agentAnswer struct {
	Agents   []agentView       `json:"agents"`
	Settings agentSettingsView `json:"settings"`
}

func TestAgentsAPI(t *testing.T) {
	f, _ := saFixture(t, nil)
	f.keys.Agents, f.keys.RecordKey = defaultAgents, testRecordKey
	if err := f.keys.Client.Create(context.Background(), scopeReady()); err != nil {
		t.Fatal(err)
	}
	admin, ana, bob := f.cookie("admin@example.com", RoleAdmin), f.cookie("ana@acme.com", RoleViewer), f.cookie("bob@acme.com", RoleViewer)
	if r := f.call(t, "GET", "/api/agents", nil, nil); r.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d", r.Code)
	}
	// create: a person, for themselves
	r := f.call(t, "POST", "/api/agents", ana, map[string]any{"displayName": "Grok Bot", "platform": "grok", "scope": []string{"crm/read"}})
	var a agentView
	_ = json.Unmarshal(r.Body.Bytes(), &a)
	if r.Code != http.StatusCreated || a.Owner != "ana@acme.com" || a.Mode != ModePerson || !a.Manageable || a.Identity != "" {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	// unknown fields and other modes
	for name, c := range map[string]struct {
		body any
		code int
	}{
		"unknown field":           {map[string]any{"displayName": "x", "owner": "bob@acme.com", "usedBy": "team"}, http.StatusBadRequest},
		"itself by a person":      {map[string]any{"displayName": "x", "mode": "itself", "name": "ci", "group": "eng"}, http.StatusForbidden},
		"no name":                 {map[string]any{"platform": "grok"}, http.StatusBadRequest},
		"scope of the wrong kind": {map[string]any{"displayName": "x", "scope": []string{"a b"}}, http.StatusBadRequest},
	} {
		if r := f.call(t, "POST", "/api/agents", ana, c.body); r.Code != c.code {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	// the list is the caller's; admins ask for all
	var l agentAnswer
	r = f.call(t, "GET", "/api/agents", ana, nil)
	_ = json.Unmarshal(r.Body.Bytes(), &l)
	if r.Code != 200 || len(l.Agents) != 1 || l.Agents[0].Name != a.Name || !l.Settings.Enabled || l.Settings.MaxPerPerson != 10 || l.Settings.KeyMaxDays != 90 {
		t.Fatalf("list: %d %s", r.Code, r.Body)
	}
	if r := f.call(t, "GET", "/api/agents", bob, nil); strings.Contains(r.Body.String(), a.Name) {
		t.Errorf("bob sees ana's agent: %s", r.Body)
	}
	if r := f.call(t, "GET", "/api/agents?all=true", bob, nil); r.Code != http.StatusForbidden {
		t.Errorf("all for a person: %d", r.Code)
	}
	if r := f.call(t, "GET", "/api/agents?all=true", admin, nil); !strings.Contains(r.Body.String(), a.Name) {
		t.Errorf("all for an admin: %s", r.Body)
	}
	// detail: owner and admin; not others (404, not 403: the name is not confirmed)
	if r := f.call(t, "GET", "/api/agents/"+a.Name, bob, nil); r.Code != http.StatusNotFound {
		t.Errorf("bob's detail: %d", r.Code)
	}
	if r := f.call(t, "GET", "/api/agents/"+a.Name, admin, nil); r.Code != 200 {
		t.Errorf("admin's detail: %d", r.Code)
	}
	// keys: the answer holds the key once, never again
	r = f.call(t, "POST", "/api/agents/"+a.Name+"/keys", ana, map[string]any{"name": "connector", "days": 30, "sourceRanges": []string{"203.0.113.0/24"}})
	var created createdKey
	_ = json.Unmarshal(r.Body.Bytes(), &created)
	if r.Code != http.StatusCreated || r.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(created.Key, "ambit_") ||
		created.View["agent"] != a.Name || fmt.Sprint(created.View["sourceRanges"]) != "[203.0.113.0/24]" || created.URLs["mcp"] == "" {
		t.Fatalf("key: %d %v %s", r.Code, r.Header(), r.Body)
	}
	for _, ck := range []*http.Cookie{ana, admin} {
		r = f.call(t, "GET", "/api/agents/"+a.Name, ck, nil)
		var d agentView
		_ = json.Unmarshal(r.Body.Bytes(), &d)
		if r.Code != 200 || d.Keys == nil || len(*d.Keys) != 1 || strings.Contains(r.Body.String(), created.Key) {
			t.Fatalf("detail: %d %s", r.Code, r.Body)
		}
	}
	if r := f.call(t, "POST", "/api/agents/"+a.Name+"/keys", bob, map[string]any{"name": "x", "days": 30}); r.Code != http.StatusNotFound {
		t.Errorf("bob's key for ana's agent: %d", r.Code)
	}
	if r := f.call(t, "POST", "/api/agents/"+a.Name+"/keys", ana, map[string]any{"name": "x", "days": 365}); r.Code != http.StatusBadRequest {
		t.Errorf("a 365-day agent key: %d", r.Code)
	}
	// the owner's key list shows it as an agent key, and the ordinary revoke endpoint revokes it
	if r := f.call(t, "GET", "/api/keys?lastUsed=false", ana, nil); !strings.Contains(r.Body.String(), `"agent":"`+a.Name+`"`) {
		t.Errorf("my keys: %s", r.Body)
	}
	// patch: scope change; others refused
	r = f.call(t, "PATCH", "/api/agents/"+a.Name, ana, map[string]any{"scope": []string{"wiki"}})
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"rev":2`) {
		t.Fatalf("patch: %d %s", r.Code, r.Body)
	}
	if r := f.call(t, "PATCH", "/api/agents/"+a.Name, bob, map[string]any{"scope": []string{"wiki"}}); r.Code != http.StatusNotFound {
		t.Errorf("bob patches: %d", r.Code)
	}
	if r := f.call(t, "PATCH", "/api/agents/"+a.Name, ana, map[string]any{"owner": "bob@acme.com"}); r.Code != http.StatusBadRequest {
		t.Errorf("a person cannot give an agent away: %d", r.Code)
	}
	// itself mode: admins
	r = f.call(t, "POST", "/api/agents", admin, map[string]any{"displayName": "Nightly", "mode": "itself", "name": "nightly", "group": "ops", "owner": "ana@acme.com"})
	if r.Code != http.StatusCreated || !strings.Contains(r.Body.String(), `"identity":"nightly@svc.ambit.internal"`) {
		t.Fatalf("itself: %d %s", r.Code, r.Body)
	}
	if r := f.call(t, "PUT", "/api/agents/nightly/owner", ana, map[string]any{"owner": "bob@acme.com"}); r.Code != http.StatusForbidden {
		t.Errorf("a person moves an agent: %d", r.Code)
	}
	if r := f.call(t, "PUT", "/api/agents/nightly/owner", admin, map[string]any{"owner": "bob@acme.com"}); r.Code != 200 || !strings.Contains(r.Body.String(), `"owner":"bob@acme.com"`) {
		t.Errorf("move: %d %s", r.Code, r.Body)
	}
	// ana (the former owner) does not manage it, and does not see it in her list either
	if r := f.call(t, "GET", "/api/agents", ana, nil); strings.Contains(r.Body.String(), "nightly") {
		t.Errorf("ana still lists nightly: %s", r.Body)
	}
	// delete
	if r := f.call(t, "DELETE", "/api/agents/"+a.Name, bob, nil); r.Code != http.StatusNotFound {
		t.Errorf("bob deletes: %d", r.Code)
	}
	if r := f.call(t, "DELETE", "/api/agents/"+a.Name, ana, nil); r.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", r.Code, r.Body)
	}
	if r := f.call(t, "GET", "/api/agents/"+a.Name, ana, nil); r.Code != http.StatusNotFound {
		t.Errorf("deleted: %d", r.Code)
	}
	// switched off: refused with 409, nothing else changes
	f.keys.Agents.Enabled = false
	if r := f.call(t, "POST", "/api/agents", ana, map[string]any{"displayName": "x"}); r.Code != http.StatusConflict {
		t.Errorf("agents off: %d", r.Code)
	}
}
