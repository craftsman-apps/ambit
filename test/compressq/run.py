#!/usr/bin/env python3
"""Real-client check of MCP result compression (issue #205, design sections 8.4 and 8.5). Standard library only.

It drives opencode or codex against an Ambit gateway whose Server `cq` serves the fixtures of test/compressq
(kubectl apply -f test/compressq/objects.yaml), through a small capture proxy that sits between the client and the
gateway. The proxy forwards each model request unchanged and records sizes and the provider's usage numbers, never
the text of a prompt, a tool result or an answer.

  run.py setup              sign in as the bootstrap admin, create a key, apply the fixtures      (writes OUT/key)
  run.py content            which of content[].text and structuredContent does the client send? (tool `both`)
  run.py tokens             one tool-heavy task, modes off/lossless/shorten, N runs each
  run.py quality            the 20 questions, modes off/lossless/shorten, N runs each
  run.py report             tables from OUT/*.jsonl

Environment: KUBECONFIG (cluster ambit-q), AMBIT=path of a `ambit` binary (for `local password`).
Run it only while you hold /tmp/ambit-kind.lock (CLAUDE.md).
"""
import argparse, concurrent.futures as cf, http.client, http.server, json, os, re, shutil, socketserver
import statistics, subprocess, sys, tempfile, threading, time, urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
MODES = ["off", "lossless", "shorten"]
LLM_HOST, MCP_HOST, CONSOLE_HOST = "llm.localtest.me", "mcp.localtest.me", "console.localtest.me"
G = "ambitproject.dev"

A = argparse.ArgumentParser()
A.add_argument("cmd", choices=["setup", "content", "tokens", "quality", "report", "probe"])
A.add_argument("--out", required=True, help="directory for keys, logs and results (not in the repository)")
A.add_argument("--client", choices=["opencode", "codex"], default="opencode")
A.add_argument("--gateway-port", type=int, default=18080)
A.add_argument("--proxy-port", type=int, default=19090)
A.add_argument("--model", default="openai/gpt-4.1-mini")
A.add_argument("--runs", type=int, default=3)
A.add_argument("--modes", default=",".join(MODES))
A.add_argument("--jobs", type=int, default=3, help="client runs at the same time (quality)")
A.add_argument("--questions", default="", help="comma separated ids, default all")
A.add_argument("--namespace", default="ambit-system")
A.add_argument("--codemode", action="store_true", help="opencode 2.x: leave MCP tools in Code Mode (the default of opencode; results stay in its sandbox)")
A.add_argument("--timeout", type=int, default=420, help="seconds per client run")
ARGS = A.parse_args()
OUT = ARGS.out
os.makedirs(OUT, exist_ok=True)


# ---------- plain HTTP to the gateway (Host header chooses the route; no DNS needed) ----------
def http_call(host, method, path, body=None, headers=None, cookies=None):
    c = http.client.HTTPConnection("127.0.0.1", ARGS.gateway_port, timeout=120)
    h = {"Host": f"{host}:{ARGS.gateway_port}", "Accept-Encoding": "identity"}
    h.update(headers or {})
    if cookies:
        h["Cookie"] = "; ".join(f"{k}={v}" for k, v in cookies.items())
    c.request(method, path, body, h)
    r = c.getresponse()
    data = r.read()
    for k, v in r.getheaders():
        if k.lower() == "set-cookie" and cookies is not None:
            n, _, rest = v.partition("=")
            val = rest.split(";", 1)[0]
            cookies.pop(n, None) if val == "" else cookies.__setitem__(n, val)
    return r.status, dict((k.lower(), v) for k, v in r.getheaders()), data


def follow(host, method, path, body=None, headers=None, cookies=None):
    for _ in range(20):
        st, hd, data = http_call(host, method, path, body, headers, cookies)
        if st in (301, 302, 303, 307) and "location" in hd:
            u = urllib.parse.urlparse(urllib.parse.urljoin(f"http://{host}:{ARGS.gateway_port}{path}", hd["location"]))
            host, path, method, body = u.hostname, u.path + (f"?{u.query}" if u.query else ""), "GET", None
            headers = None
            continue
        return st, hd, data, host
    raise SystemExit("too many redirects")


def kubectl(*a, inp=None, check=True):
    r = subprocess.run(["kubectl", *a], input=inp, capture_output=True, text=True)
    if check and r.returncode:
        raise SystemExit(f"kubectl {a[0]} failed: {r.stderr.strip()[:300]}")
    return r.stdout


def key():
    return open(os.path.join(OUT, "key")).read().strip()


# ---------- MCP through the gateway, for mode probes ----------
def mcp_call(tool, args=None):
    h = {"Authorization": f"Bearer {key()}", "Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    st, hd, _ = http_call(MCP_HOST, "POST", "/mcp", json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
        "protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "compressq", "version": "1"}}}), h)
    sid = hd.get("mcp-session-id")
    h["Mcp-Session-Id"] = sid
    http_call(MCP_HOST, "POST", "/mcp", json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}), h)
    st, _, data = http_call(MCP_HOST, "POST", "/mcp", json.dumps({"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": tool, "arguments": args or {}}}), h)
    for line in data.decode().splitlines():
        if line.startswith("data: "):
            return json.loads(line[6:])
    return json.loads(data)


def tool_result(tool):
    return mcp_call(tool)["result"]


def set_mode(mode):
    kubectl("-n", ARGS.namespace, "patch", f"servers.{G}", "cq", "--type", "merge", "-p",
            json.dumps({"spec": {"resultCompression": {"mode": mode}}}))
    # The guard reads a cache; poll until the fixtures show the mode (issues: JSON shrinks; log: marker).
    end = time.time() + 150
    while time.time() < end:
        try:
            iss = len(tool_result("cq_issues")["content"][0]["text"].encode())
            log = tool_result("cq_log")["content"][0]["text"]
            ok = {"off": iss > 20000 and "[ambit:" not in log,
                  "lossless": iss < 16000 and "[ambit:" not in log,
                  "shorten": iss < 16000 and "[ambit:" in log}[mode]
            if ok:
                return
        except Exception as e:  # the gateway may reload
            last = e
        time.sleep(3)
    raise SystemExit(f"mode {mode} did not take effect in 150 s")


# ---------- capture proxy ----------
USAGE_LOCK = threading.Lock()


def usage_of(data):
    """Last usage object of a JSON or SSE answer: chat completions and Responses API names."""
    objs = []
    text = data.decode("utf-8", "replace")
    if text.lstrip().startswith("{"):
        try:
            objs.append(json.loads(text))
        except ValueError:
            pass
    for line in text.splitlines():
        if line.startswith("data: ") and line[6:].strip() not in ("", "[DONE]"):
            try:
                objs.append(json.loads(line[6:]))
            except ValueError:
                pass
    u = None
    for o in objs:
        cand = o.get("usage") or (o.get("response") or {}).get("usage")
        if cand:
            u = cand
    if not u:
        return None
    inp = u.get("prompt_tokens", u.get("input_tokens"))
    out = u.get("completion_tokens", u.get("output_tokens"))
    det = u.get("prompt_tokens_details") or u.get("input_tokens_details") or {}
    return {"input": inp, "output": out, "cached": det.get("cached_tokens"), "cost": u.get("cost")}


def tool_sizes(req):
    """Sizes of the tool results in a model request (chat messages or Responses input items)."""
    sizes = []

    def size(v):
        if isinstance(v, str):
            return len(v.encode())
        if isinstance(v, list):
            return sum(size(p.get("text", "") if isinstance(p, dict) else p) for p in v)
        return len(json.dumps(v).encode())
    for m in req.get("messages") or []:
        if m.get("role") == "tool":
            sizes.append(size(m.get("content")))
    inp = req.get("input")
    if isinstance(inp, list):
        for it in inp:
            if isinstance(it, dict) and it.get("type") in ("function_call_output", "custom_tool_call_output"):
                sizes.append(size(it.get("output")))
    return sizes


def shape(req):
    """Structure of a request without any text: roles, part types and sizes (to see where a client puts tool results)."""
    def part(p):
        if isinstance(p, dict):
            return {k: (len(v) if isinstance(v, str) else type(v).__name__) for k, v in p.items()}
        return len(p) if isinstance(p, str) else type(p).__name__
    out = []
    for m in (req.get("messages") or req.get("input") or []):
        if isinstance(m, dict):
            c = m.get("content", m.get("output"))
            out.append({"role": m.get("role", m.get("type")), "keys": sorted(m), "content": [part(x) for x in c] if isinstance(c, list) else part(c)})
    return out


class Proxy(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        self.go()

    def do_POST(self):
        self.go()

    def go(self):
        m = re.match(r"^/([^/]+)(/.*)$", self.path)
        label, rest = m.groups()
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else b""
        if os.environ.get("CQ_DUMP") and body:  # debugging with fixtures only: raw request bodies
            open(os.path.join(OUT, "dump-" + label + ".json"), "wb").write(body)
        rec = {"label": label, "path": rest.split("?")[0], "method": self.command, "req_bytes": len(body), "t": time.time()}
        try:
            req = json.loads(body) if body else {}
            rec["tool_results"] = tool_sizes(req)
            rec["shape"] = shape(req)
            # Marker counts only (never the text): how often the model request holds the text or structured form.
            rec["txt_marks"], rec["struct_marks"] = body.count(b"TXTMARK alpha"), body.count(b"STRUCTMARK beta")
        except ValueError:
            pass
        h = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "content-length", "connection", "accept-encoding", "transfer-encoding")}
        h.update({"Host": f"{LLM_HOST}:{ARGS.gateway_port}", "Accept-Encoding": "identity"})
        c = http.client.HTTPConnection("127.0.0.1", ARGS.gateway_port, timeout=600)
        try:
            c.request(self.command, rest, body or None, h)
            r = c.getresponse()
            data = r.read()
            status, hdrs = r.status, r.getheaders()
        except Exception as e:
            status, hdrs, data = 502, [], f"proxy error: {e}".encode()
        rec["status"] = status
        rec["usage"] = usage_of(data) if rec["path"].endswith(("/chat/completions", "/responses")) else None
        with USAGE_LOCK:
            with open(os.path.join(OUT, "proxy.jsonl"), "a") as f:
                f.write(json.dumps(rec) + "\n")
        self.send_response(status)
        for k, v in hdrs:
            if k.lower() not in ("transfer-encoding", "connection", "content-length", "content-encoding"):
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class Threaded(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


def start_proxy():
    s = Threaded(("127.0.0.1", ARGS.proxy_port), Proxy)
    threading.Thread(target=s.serve_forever, daemon=True).start()
    return s


def proxy_records(label):
    p = os.path.join(OUT, "proxy.jsonl")
    if not os.path.exists(p):
        return []
    with USAGE_LOCK:
        return [r for r in map(json.loads, open(p)) if r["label"] == label]


# ---------- clients ----------
def client_env(tmp, label):
    """A temporary HOME and config for one run. Never the user's own config."""
    home = os.path.join(tmp, "home")
    for d in ("home", "config", "data", "state", "cache"):
        os.makedirs(os.path.join(tmp, d), exist_ok=True)
    env = {"PATH": os.environ["PATH"], "HOME": home, "XDG_CONFIG_HOME": os.path.join(tmp, "config"), "XDG_DATA_HOME": os.path.join(tmp, "data"),
           "XDG_STATE_HOME": os.path.join(tmp, "state"), "XDG_CACHE_HOME": os.path.join(tmp, "cache"), "AMBIT_API_KEY": key(),
           "CODEX_HOME": os.path.join(tmp, "codex"), "NO_COLOR": "1"}
    model = ARGS.model
    base = f"http://127.0.0.1:{ARGS.proxy_port}/{label}/v1"
    mcp_url = f"http://{MCP_HOST}:{ARGS.gateway_port}/mcp"
    if ARGS.client == "opencode":
        cfg = {"$schema": "https://opencode.ai/config.json", "model": f"ambit/{model}",
               "provider": {"ambit": {"npm": "@ai-sdk/openai-compatible", "name": "Ambit",
                                      "options": {"baseURL": base, "apiKey": "{env:AMBIT_API_KEY}"}, "models": {model: {"name": model}}}},
               "mcp": {"servers": {"cq": {"type": "remote", "url": mcp_url, "headers": {"Authorization": "Bearer {env:AMBIT_API_KEY}"}, "oauth": False,
                                  "codemode": ARGS.codemode}}},
               "autoupdate": False, "share": "disabled"}
        os.makedirs(os.path.join(tmp, "config", "opencode"), exist_ok=True)
        json.dump(cfg, open(os.path.join(tmp, "config", "opencode", "opencode.json"), "w"))
    else:
        os.makedirs(env["CODEX_HOME"], exist_ok=True)
        open(os.path.join(env["CODEX_HOME"], "config.toml"), "w").write(f'''model = "{model}"
model_provider = "ambit"

[model_providers.ambit]
name = "Ambit"
base_url = "{base}"
env_key = "AMBIT_API_KEY"
wire_api = "responses"

[mcp_servers.cq]
url = "{mcp_url}"
bearer_token_env_var = "AMBIT_API_KEY"
default_tools_approval_mode = "approve"
''')
    return env


def run_client(label, prompt):
    """One client run. Returns (final answer text, exit code, seconds)."""
    tmp = tempfile.mkdtemp(prefix="cq-")
    work = os.path.join(tmp, "work")
    os.makedirs(work)
    env = client_env(tmp, label)
    t0 = time.time()
    try:
        if ARGS.client == "opencode":
            cmd = ["opencode", "run", "--standalone", "--auto", "--format", "json", "-m", f"ambit/{ARGS.model}", prompt]
            r = subprocess.run(cmd, cwd=work, env=env, capture_output=True, text=True, timeout=ARGS.timeout, stdin=subprocess.DEVNULL)
            answer = opencode_answer(r.stdout)
        else:
            last = os.path.join(tmp, "last.txt")
            cmd = ["codex", "exec", "--skip-git-repo-check", "--sandbox", "read-only", "-C", work, "-o", last, prompt]
            r = subprocess.run(cmd, cwd=work, env=env, capture_output=True, text=True, timeout=ARGS.timeout, stdin=subprocess.DEVNULL)
            answer = open(last).read() if os.path.exists(last) else ""
        with open(os.path.join(OUT, "stderr.log"), "a") as f:  # tails only, for debugging; they hold no key
            f.write(f"--- {label} rc={r.returncode}\n{r.stderr[-600:]}\n")
        return answer.strip(), r.returncode, time.time() - t0
    except subprocess.TimeoutExpired:
        return "", -1, time.time() - t0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def opencode_answer(stdout):
    """The text parts of the last assistant message in opencode's JSON event stream."""
    parts = []
    for line in stdout.splitlines():
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if e.get("type") == "text":
            t = (e.get("part") or {}).get("text") or e.get("text")
            if t:
                parts.append(t)
    return parts[-1] if parts else ""


# ---------- grading ----------
def norm(s):
    return re.sub(r"[*`,]", "", s.lower())


def grade(q, answer):
    a = norm(answer)
    if q["class"] == "omitted" and re.search(q["notShown"], a):
        return "not_shown"
    return "correct" if all(norm(e) in a for e in q["expect"]) else "wrong"


def load_questions():
    out = subprocess.run(["go", "run", "./test/compressq", "-questions"], cwd=os.path.join(HERE, "..", ".."), capture_output=True, text=True)
    if out.returncode:
        raise SystemExit("cannot read the questions: " + out.stderr[:300])
    qs = json.loads(out.stdout)
    want = [x for x in ARGS.questions.split(",") if x]
    return [q for q in qs if not want or q["id"] in want]


def append(name, rec):
    with open(os.path.join(OUT, name), "a") as f:
        f.write(json.dumps(rec) + "\n")


# ---------- commands ----------
def cmd_setup():
    ambit = os.environ.get("AMBIT", "ambit")
    pw = subprocess.run([ambit, "local", "password", "--name", "ambit-q"], capture_output=True, text=True).stdout.strip()
    if not pw:
        raise SystemExit("no admin password (is KUBECONFIG set to the ambit-q cluster?)")
    cookies = {}
    st, hd, body, host = follow(CONSOLE_HOST, "GET", "/auth/login?connector=local", cookies=cookies)
    m = re.search(r'action="([^"]*)"', body.decode())
    if not m:
        raise SystemExit(f"no login form (HTTP {st})")
    action = m.group(1).replace("&amp;", "&")
    if action.startswith("http"):
        u = urllib.parse.urlparse(action)
        action = u.path + (f"?{u.query}" if u.query else "")
    form = urllib.parse.urlencode({"login": "admin@ambit.local", "password": pw})
    st, hd, body, host = follow(host, "POST", action, form, {"Content-Type": "application/x-www-form-urlencoded"}, cookies)
    st, _, data = http_call(CONSOLE_HOST, "POST", "/api/keys", json.dumps({"name": "compressq", "scope": "both", "days": 30}),
                            {"Content-Type": "application/json"}, cookies)
    if st != 201:
        raise SystemExit(f"create key: HTTP {st} {data[:200]!r}")
    fd = os.open(os.path.join(OUT, "key"), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    os.write(fd, json.loads(data)["key"].encode())
    os.close(fd)
    here = os.path.join(HERE, "objects.yaml")
    kubectl("-n", ARGS.namespace, "apply", "-f", here)
    kubectl("-n", ARGS.namespace, "rollout", "status", "deploy/compressq", "--timeout=180s")
    kubectl("-n", ARGS.namespace, "wait", f"servers.{G}", "cq", "--for=condition=Ready", "--timeout=180s")
    print("key stored in", os.path.join(OUT, "key"))


def cmd_probe():
    for t in ("cq_issues", "cq_log", "cq_tests", "cq_csv"):
        r = tool_result(t)
        print(t, [len(c.get("text", "")) for c in r["content"]], "structured" if "structuredContent" in r else "")


def cmd_content():
    start_proxy()
    set_mode("off")
    prompt = "Use the MCP tool cq_both (no arguments). Then answer in one short line how many lines its result had."
    for i in range(ARGS.runs):
        label = f"content-{ARGS.client}-{i}"
        ans, rc, secs = run_client(label, prompt)
        recs = [r for r in proxy_records(label) if r["path"].endswith(("/chat/completions", "/responses"))]
        last = recs[-1] if recs else {}
        rec = {"client": ARGS.client, "run": i, "rc": rc, "requests": len(recs), "answer": ans[:200], "tool_result_bytes": last.get("tool_results"),
               "txt_marks_in_last_request": last.get("txt_marks"), "struct_marks_in_last_request": last.get("struct_marks")}
        append("content.jsonl", rec)
        print(rec)


TOKEN_PROMPT = ("Use only the cq_ MCP tools. Call these five MCP tools one at a time, in this order, and wait for each result before the next call; "
                "no arguments: cq_log, cq_tests, cq_issues, cq_rows, cq_pods. Then answer in five short numbered lines: "
                "(1) how many ERROR lines the log has, (2) the names of the failed tests, (3) how many issues have the label bug, "
                "(4) the amount of the row with id 117, (5) the pod with restartCount 7.")
TOKEN_EXPECT = ["8", "testparseheader", "testretrybackoff", "testcacheevict", "6", "829.87", "web-23"]


def cmd_tokens():
    start_proxy()
    for mode in ARGS.modes.split(","):
        set_mode(mode)
        have = len([r for r in rows("tokens.jsonl") if r["client"] == ARGS.client and r["mode"] == mode and r.get("valid")]) if os.path.exists(os.path.join(OUT, "tokens.jsonl")) else 0
        attempt = 0
        while have < ARGS.runs and attempt < ARGS.runs * 3:
            i, attempt = attempt, attempt + 1
            label = f"tok-{ARGS.client}-{mode}-{int(time.time())}-{i}"
            ans, rc, secs = run_client(label, TOKEN_PROMPT)
            recs = [r for r in proxy_records(label) if r["path"].endswith(("/chat/completions", "/responses")) and r.get("usage")]
            last = ([r["tool_results"] for r in recs] or [[]])[-1]
            # valid = the client got all five results (a run where the MCP tools were not connected in time is not a measurement)
            valid = rc == 0 and len([n for n in last if n >= 400]) >= 5
            have += valid
            rec = {"client": ARGS.client, "mode": mode, "run": i, "rc": rc, "secs": round(secs), "requests": len(recs), "valid": valid,
                   "input": sum(r["usage"]["input"] or 0 for r in recs), "output": sum(r["usage"]["output"] or 0 for r in recs),
                   "cached": sum(r["usage"]["cached"] or 0 for r in recs), "cost": sum(r["usage"].get("cost") or 0 for r in recs),
                   "answer_ok": all(norm(e) in norm(ans) for e in TOKEN_EXPECT), "answer": ans[:500], "last_tool_results": last}
            append("tokens.jsonl", rec)
            print({k: v for k, v in rec.items() if k != "answer"}, flush=True)


def cmd_quality():
    start_proxy()
    qs = load_questions()
    for mode in ARGS.modes.split(","):
        set_mode(mode)

        def one(job):
            q, i = job
            for attempt in range(3):  # a run where the MCP tools were not connected in time is not a measurement: try again
                label = f"q-{ARGS.client}-{mode}-{i}-{q['id']}-{int(time.time())}-{attempt}"
                ans, rc, secs = run_client(label, q["prompt"])
                recs = [r for r in proxy_records(label) if r["path"].endswith(("/chat/completions", "/responses")) and r.get("usage")]
                last = ([r["tool_results"] for r in recs] or [[]])[-1]
                if rc == 0 and any(n >= 300 for n in last):
                    break
            rec = {"client": ARGS.client, "mode": mode, "run": i, "q": q["id"], "class": q["class"], "rc": rc, "grade": grade(q, ans),
                   "attempts": attempt + 1, "tool_called": any(n >= 300 for n in last), "requests": len(recs),
                   "input": sum(r["usage"]["input"] or 0 for r in recs), "cost": sum(r["usage"].get("cost") or 0 for r in recs),
                   "answer": ans[:300]}
            append("quality.jsonl", rec)
            return rec
        done = {(r["q"], r["run"]) for r in rows("quality.jsonl") if r["client"] == ARGS.client and r["mode"] == mode and r.get("tool_called")}
        jobs = [(q, i) for i in range(ARGS.runs) for q in qs if (q["id"], i) not in done]  # a second pass fills what failed
        with cf.ThreadPoolExecutor(ARGS.jobs) as ex:
            for rec in ex.map(one, jobs):
                print(mode, rec["q"], rec["run"], rec["grade"], rec["rc"], flush=True)


def rows(name):
    p = os.path.join(OUT, name)
    return [json.loads(l) for l in open(p)] if os.path.exists(p) else []


def cmd_report():
    t = rows("tokens.jsonl")
    for client in sorted({r["client"] for r in t}):
        print(f"\n### Input tokens per session, {client}\n\n| Mode | Runs | Median | Range | Saving vs off (median) | Requests |\n|---|---|---|---|---|---|")
        base = None
        for mode in MODES:
            xs = [r for r in t if r["client"] == client and r["mode"] == mode and r.get("valid", r["rc"] == 0) and r["input"]]
            if not xs:
                continue
            v = sorted(r["input"] for r in xs)
            med = statistics.median(v)
            base = med if mode == "off" else base
            sv = "-" if mode == "off" or not base else f"{(1 - med / base) * 100:.0f}%"
            print(f"| {mode} | {len(v)} | {med:,.0f} | {v[0]:,} to {v[-1]:,} | {sv} | {statistics.median(r['requests'] for r in xs):.0f} |")
    q = rows("quality.jsonl")
    for client in sorted({r["client"] for r in q}):
        print(f"\n### Answer quality, {client}: correct runs of 3 (majority = 2 or more)\n\n| Q | Class | off | lossless | shorten | Gate |\n|---|---|---|---|---|---|")
        lost_ll, lost_sh = [], []
        for qid in sorted({r["q"] for r in q if r["client"] == client}):
            cell, maj, cls = {}, {}, ""
            for mode in MODES:
                g = [r for r in q if r["client"] == client and r["q"] == qid and r["mode"] == mode and r.get("tool_called")]
                g = list({r["run"]: r for r in g}.values())  # one record per run (the last valid one)
                if not g:
                    continue
                cls = g[0]["class"]
                good = sum(r["grade"] == "correct" for r in g)
                ns = sum(r["grade"] == "not_shown" for r in g)
                cell[mode] = f"{good}/{len(g)}" + (f" (+{ns} not shown)" if ns else "")
                maj[mode] = good * 2 > len(g) or (cls == "omitted" and (good + ns) * 2 > len(g))
            gate = ""
            if maj.get("off") and "lossless" in maj and not maj["lossless"]:
                gate, _ = "LOST in lossless", lost_ll.append(qid)
            if maj.get("off") and "shorten" in maj and not maj["shorten"] and cls in ("failure", "error", "count"):
                gate += " LOST in shorten"
                lost_sh.append(qid)
            print(f"| {qid} | {cls} | {cell.get('off', '-')} | {cell.get('lossless', '-')} | {cell.get('shorten', '-')} | {gate or 'ok'} |")
        print(f"\nQuestions lost in lossless: {lost_ll or 'none'}. Failure/error/count questions lost in shorten: {lost_sh or 'none'}.")


{"setup": cmd_setup, "probe": cmd_probe, "content": cmd_content, "tokens": cmd_tokens, "quality": cmd_quality, "report": cmd_report}[ARGS.cmd]()
