#!/usr/bin/env python3
"""The people's side of the PostgreSQL operations checks (test/stepoauth.sh check 16 and 17 run this, one mode at a time). Standard
library only. The database side (dump, restore, a switched Service, a stopped pod) is in the shell script; this file makes the grants that
the database must keep, and looks at what a platform and a person would see.

Modes (python3 dbops.py MODE ...), environment as gateway.py:
  grants N FILE            N connections of the person bob (each a new client and agent): FILE = [{cid, refresh, access}, ...]
  refresh FILE INDEX       one refresh of grant INDEX; prints "HTTP-status error-code"; on 200 the new refresh token goes into FILE
  mcp FILE INDEX           an MCP tool list with the access token of grant INDEX at every public pod; prints the statuses
  abandon                  a sign-in that stops at the company's login page: leaves one login request in Hydra; prints the client ID
  dcr N                    N clients registered like a platform does; prints their IDs
  probe SECONDS FILE       every 0.5 s a JWKS request and a refresh with a made-up token (a real client); FILE = [[epoch seconds, jwks, token], ...]
  report FILE              the errors of a probe file: count, first and last time (epoch seconds), and whether the last probe was fine
  load SECONDS THREADS FILE PORT...  made-up refresh requests straight to Hydra pods (http://127.0.0.1:PORT): FILE = latencies and statuses
"""
import os
src = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "gateway.py")).read().split("# 1 · no token")[0]
exec(src)
import threading

mode = sys.argv[1]
if mode == "grants":
    n, out = int(sys.argv[2]), sys.argv[3]
    gs = []
    for i in range(n):
        cid, tok = connect("bob", ("time",), name=f"Platform {i}")
        gs.append({"cid": cid, "refresh": tok["refresh_token"], "access": tok["access_token"]})
    json.dump(gs, open(out, "w"))
    print(f"{n} grants")
elif mode == "refresh":
    gs = json.load(open(sys.argv[2])); i = int(sys.argv[3])
    st, j = token(gs[i]["cid"], grant_type="refresh_token", refresh_token=gs[i]["refresh"])
    if st == 200:
        gs[i]["refresh"] = j["refresh_token"]; gs[i]["access"] = j["access_token"]
        json.dump(gs, open(sys.argv[2], "w"))
    print(st, j.get("error", ""))
elif mode == "mcp":
    gs = json.load(open(sys.argv[2])); i = int(sys.argv[3])
    print(" ".join(str(tools(p, gs[i]["access"])[0]) for p in PORTS))
elif mode == "abandon":
    reg = register(REDIRECT, "Abandoned")[2]
    b = Browser()
    b.follow(authorize_url(reg["client_id"], pkce()[1], "abandoned"))  # to the company's login page, and no further
    print(reg["client_id"])
elif mode == "dcr":
    for i in range(int(sys.argv[2])):
        print(register(REDIRECT, f"Cleanup {i}")[2]["client_id"])
elif mode == "probe":
    secs, out = float(sys.argv[2]), sys.argv[3]
    st, _, reg = register(REDIRECT, "Probe")
    cid = reg["client_id"]
    rows, end = [], time.time() + secs
    while time.time() < end:
        t = time.time()
        try: a = Browser().req("GET", base_url("/.well-known/jwks.json"))[0]
        except Exception: a = 0
        b_ = token(cid, grant_type="refresh_token", refresh_token="ory_rt_made-up-for-the-probe")[0]
        rows.append([round(t, 2), a, b_])
        json.dump(rows, open(out, "w"))
        time.sleep(0.5)
    print(f"{len(rows)} probes")
elif mode == "report":
    rows = json.load(open(sys.argv[2]))
    bad = [r for r in rows if r[1] != 200 or r[2] not in (400, 401)]
    codes = sorted({(r[1], r[2]) for r in rows})
    if bad:
        print(f"probes={len(rows)} errors={len(bad)} first={bad[0][0]} last={bad[-1][0]} end_ok={rows[-1] not in bad} codes={codes}")
    else:
        print(f"probes={len(rows)} errors=0 first=0 last=0 end_ok=True codes={codes}")
elif mode == "load":
    secs, nthreads, out, ports = float(sys.argv[2]), int(sys.argv[3]), sys.argv[4], [int(p) for p in sys.argv[5:]]
    import http.client
    lat, codes, lock = [], {}, threading.Lock()
    end = time.time() + secs

    def worker(k):
        port = ports[k % len(ports)]
        c = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
        body = "grant_type=refresh_token&client_id=load&refresh_token=ory_rt_made-up-for-the-load"
        while time.time() < end:
            t = time.time()
            try:
                c.request("POST", "/oauth2/token", body, {"Content-Type": "application/x-www-form-urlencoded", "X-Forwarded-Proto": "https"})
                r = c.getresponse(); r.read(); code = r.status
            except Exception:
                code = 0; c = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
            with lock:
                lat.append(time.time() - t); codes[code] = codes.get(code, 0) + 1
    ts = [threading.Thread(target=worker, args=(k,)) for k in range(nthreads)]
    [t.start() for t in ts]; [t.join() for t in ts]
    lat.sort()
    res = {"requests": len(lat), "codes": codes, "p50_ms": round(lat[len(lat) // 2] * 1000), "p95_ms": round(lat[int(len(lat) * .95)] * 1000)}
    json.dump(res, open(out, "w")); print(json.dumps(res))
else:
    sys.exit("unknown mode " + mode)
