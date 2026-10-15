"""Helpers of test/stepaudit.sh steps 1-8 (audit-only mode on kind). Standard library only.

  loop OUT LLM MCP KEYCLOAK U V          every 0.5 s: U's model call, U's tool call, V's model call; one JSON line each
  switch NS MODE T0 OUT DUMP_URL...      follow the new controller's log during a switch; check every /config_dump
  routed RECORDS                         step 4: routed records name the answering model; marks allow / other-model
  audit4 RECORDS                         step 4: U refuse:model and refuse:tool, V allow, tools/list n/a
  analyze LOOP RECORDS SWITCH_A SWITCH_E steps 6 and 7: the loop against the records
"""
import json
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# The marks of a served call; "n/a" (not a call) on one is a generator miss, counted as missing.
MARKS = {"allow", "refuse:model", "refuse:router", "refuse:tool", "other-model"}


def loop(out, llm, mcp, kc, u, v):
    def token(user):
        d = urllib.parse.urlencode({"grant_type": "password", "client_id": "ambit-cli-test", "username": user,
                                    "password": user + "-test-pass"}).encode()
        return json.load(urllib.request.urlopen(kc + "/realms/acme/protocol/openid-connect/token", d, timeout=5))["access_token"]

    def post(url, body, t, sid=None):
        h = {"Authorization": "Bearer " + t, "Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
        if sid:
            h["Mcp-Session-Id"] = sid
        try:
            with urllib.request.urlopen(urllib.request.Request(url, json.dumps(body).encode(), h), timeout=10) as r:
                return r.status, r.read().decode(), r.headers.get("Mcp-Session-Id")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode(), None
        except Exception as e:  # no connection: status 0
            return 0, str(e), None

    def tool(t):
        """A new session, then tools/call; returns (status, ok|error|no session, start of the call)."""
        st, _, sid = post(mcp, {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
            "protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "stepaudit", "version": "1"}}}, t)
        if st != 200 or not sid:
            return st, "no session", time.time()
        post(mcp, {"jsonrpc": "2.0", "method": "notifications/initialized"}, t, sid)
        start = time.time()
        st, body, _ = post(mcp, {"jsonrpc": "2.0", "id": 2, "method": "tools/call",
                                 "params": {"name": "time_get_current_time", "arguments": {"timezone": "UTC"}}}, t, sid)
        data = [l[6:] for l in body.splitlines() if l.startswith("data: ")] or [body]
        try:
            d = json.loads(data[0])
            ok = "result" in d and not d["result"].get("isError")
        except ValueError:
            ok = False
        return st, "ok" if ok else "error", start

    tok, got = {}, 0.0
    with open(out, "a", buffering=1) as f:
        while True:
            tick = time.time()
            if tick - got > 60:  # Keycloak access tokens live 5 minutes
                tok, got = {u: token(u), v: token(v)}, tick
            for who, kind in ((u, "model"), (u, "tool"), (v, "model")):
                start = time.time()
                if kind == "model":
                    st, _, _ = post(llm + "/v1/chat/completions", {"model": "mock/echo", "messages": [{"role": "user", "content": "hello"}]}, tok[who])
                    res = str(st)
                else:
                    st, res, start = tool(tok[who])
                f.write(json.dumps({"who": who, "kind": kind, "start": start, "end": time.time(), "status": st, "result": res}) + "\n")
            time.sleep(max(0.0, 0.5 - (time.time() - tick)))


def kubectl(*args):
    return subprocess.run(["kubectl", *args], capture_output=True, text=True).stdout


def switch(ns, mode, t0, out, *dumps):
    """Follows the controller that runs with --access-mode=MODE. When agentgateway accepted a step, measures how long
    until every gateway pod's /config_dump shows it; when the controller says the step is active, every dump must
    show it. Then waits for ambit-access-mode to publish MODE."""
    audit, t0 = mode == "audit", float(t0)

    def dump(u):
        return urllib.request.urlopen(u, timeout=5).read().decode()

    def authorization(models):
        """Per AgentgatewayModel name: (is a router, its authorization: a Model's expressions or a router's line
        conditions). models: the API objects, or the dump's "models"."""
        out = {}
        for m in models:
            if "spec" in m:  # an API object
                name, spec = m["metadata"]["name"], m["spec"]
                if "virtualModel" in spec:
                    out[name] = (True, [t.get("when") for t in spec["virtualModel"]["conditional"]["targets"]])
                else:
                    out[name] = (False, spec.get("policies", {}).get("authorization", {}).get("policy", {}).get("matchExpressions"))
            else:  # the dump: key namespace/name.namespace.route.listener (a name may hold dots)
                key, k = m["key"].split("/", 1)[1], m["kind"]
                name = key[:key.rfind(f".{ns}.")]
                if "virtual" in k:
                    out[name] = (True, [c.get("when") for c in k["virtual"]["routing"]["conditional"]])
                else:
                    out[name] = (False, k["concrete"].get("policies", {}).get("authorization", {}).get("rules", {}).get("allow"))
        return out

    def shows(step, d):
        if step == "access log":
            return ("ambit.wouldRefuse" in d) == audit
        # authorization: the time server's MCP expression is in the authorization in enforce mode; in audit mode
        # the authorization is `true` and the expression is only in the log field ambit.wouldRefuseTool.
        n = d.count('mcp.tool.target == \\"time\\"')
        if n != (1 if audit else (2 if "ambit.wouldRefuseTool" in d else 1)):
            return False
        # Every Model's and router's authorization is the one written; in audit mode a Model's is `true` (or
        # `(true) && ` the unpriced refusal).
        want = authorization(json.loads(kubectl("-n", ns, "get", "agentgatewaymodel", "-o", "json"))["items"])
        if authorization(json.loads(d).get("models", [])) != want:
            return False
        return not audit or all(e == "true" or e.startswith("(true) && ") for router, x in want.values() if not router for e in x or [])

    deadline, name = time.time() + 300, None
    while time.time() < deadline and not name:
        for p in json.loads(kubectl("-n", ns, "get", "pod", "-l", "app.kubernetes.io/name=ambit-controller", "-o", "json"))["items"]:
            args = [a for c in p["spec"]["containers"] for a in c.get("command", []) + c.get("args", [])]
            if not p["metadata"].get("deletionTimestamp") and f"--access-mode={mode}" in args and p["status"].get("phase") == "Running":
                name = p["metadata"]["name"]
        time.sleep(0.5)
    if not name:
        sys.exit("no running controller with --access-mode=" + mode)
    res, step, last = {"steps": {}}, None, "authorization" if audit else "access log"
    logs = subprocess.Popen(["kubectl", "-n", ns, "logs", "-f", name], stdout=subprocess.PIPE, text=True)
    for line in logs.stdout:
        try:
            e = json.loads(line)
        except ValueError:
            continue
        m = e.get("msg", "")
        if m == "change step written; waiting for agentgateway":
            step = e["step"]
        elif m == "agentgateway accepted the step; waiting the settle time" and step:
            accepted = time.time()
            while time.time() - accepted < 30 and not all(shows(step, dump(u)) for u in dumps):
                time.sleep(0.1)
            res["steps"].setdefault(step, {})["dump_after_accept_s"] = round(time.time() - accepted, 2)
        elif m == "change step active in the gateway":
            ok = all(shows(e["step"], dump(u)) for u in dumps)
            res["steps"].setdefault(e["step"], {})["in_every_dump_when_active"] = ok
            if not ok:
                logs.kill()
                sys.exit(f"step {e['step']} is active, but not in every /config_dump: {res}")
            if e["step"] == last:
                break
        elif m == "change not active in the gateway; stopped at this step":
            logs.kill()
            sys.exit("stalled: " + line)
    logs.kill()
    deadline = time.time() + 120
    while time.time() < deadline:
        d = json.loads(kubectl("-n", ns, "get", "configmap", "ambit-access-mode", "-o", "json"))["data"]
        if d.get("mode") == mode and not d.get("pending"):
            break
        time.sleep(0.5)
    else:
        sys.exit(f"ambit-access-mode did not publish {mode}: {res}")
    res["published_s"] = round(time.time() - t0, 1)
    with open(out, "w") as f:
        json.dump(res, f)
    print(json.dumps(res))


def rows(path):
    return [json.loads(l) for l in open(path) if l.strip()]


def routed(path):
    r = {x["who"]: x for x in rows(path)}
    m, a = r.get("mira@acme.test", {}), r.get("alice@acme.test", {})
    # Open point 1: a routed record's llm.requestModel is the model of the line that answered.
    if not (m.get("model") == "mock/echo" and m.get("mark") == "allow" and a.get("model") == "mock/echo" and a.get("mark") == "other-model"):
        sys.exit(f"routed records {r}")


def audit4(path):
    rs = rows(path)

    def has(who, pred):
        return any(r["who"] == who and pred(r) for r in rs)
    ok = (has("alice@acme.test", lambda r: r["model"] == "mock/echo" and r["mark"] == "refuse:model")
          and has("alice@acme.test", lambda r: r["method"] == "tools/call" and r["toolmark"] == "refuse:tool")
          and has("mira@acme.test", lambda r: r["model"] == "mock/echo" and r["mark"] == "allow")
          and has("alice@acme.test", lambda r: r["method"] == "tools/list" and r["toolmark"] == "n/a"))
    sys.exit(0 if ok else 1)


def analyze(loop_path, records_path, switch_audit, switch_enforce):
    loop, recs = rows(loop_path), rows(records_path)
    email = {"alice": "alice@acme.test", "mira": "mira@acme.test"}
    used, problems = set(), []
    for e in loop:  # one request per person and target per 0.5 s; 100 ms for the clocks
        e["rec"] = None
        if e["kind"] == "tool" and e["result"] == "no session":
            problems.append(f"no MCP session: {e}")
            continue
        for i, r in enumerate(recs):
            if i in used or not e["start"] - 0.1 <= float(r["t"]) <= e["end"] + 0.1:
                continue
            # A model refusal (403 from agentgateway's model router) is recorded without the caller or the model:
            # it is matched by status and time. Only alice is refused.
            refused = e["kind"] == "model" and e["status"] == 403 and r["who"] == "" and r["status"] == "403"
            if r["who"] != email[e["who"]] and not refused:
                continue
            if refused or (e["kind"] == "tool" and r["method"] == "tools/call") or (e["kind"] == "model" and r["model"] == "mock/echo"):
                used.add(i)
                e["rec"] = r
                break
        else:
            problems.append(f"no record for {e}")

    def served(e):
        return e["status"] == 200 and (e["kind"] == "model" or e["result"] == "ok")
    missing = 0
    for e in loop:
        r = e["rec"]
        if r is None or not served(e):
            continue
        mark = r["toolmark"] if e["kind"] == "tool" else r["mark"]
        if r["mode"] == "audit" and mark not in MARKS:
            missing += 1
            problems.append(f"mark missing: {e}")
        # alice is refused by the rules: served only in audit mode, and her record says so.
        if e["who"] == "alice" and not (r["mode"] == "audit" and mark.startswith("refuse:")):
            problems.append(f"alice served without an audit-mode refuse mark: {e}")
        # mira is allowed by the rules: in audit mode her record says allow.
        if e["who"] == "mira" and r["mode"] == "audit" and mark != "allow":
            problems.append(f"mira served in audit mode with mark {mark!r}, want allow: {e}")
    for e in loop:
        if e["who"] == "mira" and not served(e):
            problems.append(f"mira not served: {e}")
        if e["status"] == 0 or e["status"] >= 500:
            problems.append(f"5xx or no connection: {e}")
    for kind in ("model", "tool"):
        changes = []
        for e in loop:
            if e["who"] == "alice" and e["kind"] == kind and (not changes or changes[-1][0] != served(e)):
                changes.append((served(e), round(e["start"], 1)))
        if [c[0] for c in changes] != [False, True, False]:
            problems.append(f"alice {kind}: served changes {changes}; want refused, served, refused")
    print(f"loop entries {len(loop)}, matched to a record {sum(1 for e in loop if e['rec'])}, mark missing {missing}")
    for mode, p in (("audit", switch_audit), ("enforce", switch_enforce)):
        print(f"switch to {mode}: {open(p).read().strip()}")
    if problems:
        print("\n".join(problems[:40]))
        sys.exit(1)


if __name__ == "__main__":
    {"loop": loop, "switch": switch, "routed": routed, "audit4": audit4, "analyze": analyze}[sys.argv[1]](*sys.argv[2:])
