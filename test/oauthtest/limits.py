#!/usr/bin/env python3
"""End-to-end checks of the rate-limit buckets of the public Gateway (test/stepoauth.sh check 19 runs this, twice). Standard library only.

The Gateway is in PROXY protocol mode (frontDoor.clientAddress=proxyProtocol), so this test can be any client address it likes, as a load
balancer would tell the Gateway. What is checked (MINOR 3 of the design review, A16):
  buckets   (low limits) each bucket (discovery, sign-in, token, registration) stops at its own limit and leaves the other three open; the 401
            for a call to the MCP URL without a credential is in the discovery bucket, a call with a credential is not; headers that name another
            address (X-Forwarded-For, X-Real-IP, Forwarded, X-Ambit-Client-Address) change nothing, and a caller behind another address that writes
            the limited address into them is not limited; two Gateway pods count together (the limit is not doubled)
  second    (low limits, the ratelimit service stopped) ambit-auth's own counter stops the sign-in pages at the same limit
  bearer    (low limits) #436: the calls of a hosted platform (a bearer token in the shape of a JWT) have a bucket of their own, above the key limit: many valid tokens from
            one address pass at that limit while the keys of the same address stop at their own; a flood of junk in the shape of a JWT stops at the same limit and never
            uses up the key bucket; what is not in the shape (a key written as "Bearer Bearer ambit_...", "Bearer abc", a key header with a token) stays in the key bucket
  nat       (default limits) one shared address: how many whole sign-ins it makes in one minute before the limit, how many requests a sign-in
            costs in each bucket, and that another address is not affected

Environment: as gateway.py, plus LIM_DISCOVERY, LIM_SIGNIN, LIM_TOKEN, LIM_REGISTRATION (the limits the chart was installed with) and the mode as
the first argument."""
import os, sys
ARGV = sys.argv[:]  # the code below clears sys.argv
src = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "gateway.py")).read().split("# 1 · no token")[0]
exec(src)

MODE = ARGV[1]
LIM_MCP = int(os.environ.get("LIM_MCP", "0"))
LIM_GLOBAL = int(os.environ.get("LIM_GLOBAL", "0"))
LIM_CRED = int(os.environ.get("LIM_CRED", "0"))
LIM_BEARER = int(os.environ.get("LIM_BEARER", "0"))
OPEN = {200, 201, 400, 401, 403}  # what a request that is not limited gets here: never 404 (a route is missing) or 5xx
LIM = {"discovery": int(os.environ["LIM_DISCOVERY"]), "signin": int(os.environ["LIM_SIGNIN"]), "token": int(os.environ["LIM_TOKEN"]), "registration": int(os.environ["LIM_REGISTRATION"])}

# what one request of each bucket looks like, and the status it gets when it is NOT limited
SHAPES = {
    "discovery": ("GET", "/.well-known/oauth-authorization-server", None, {}),
    "signin": ("GET", "/error", None, {}),
    "token": ("POST", "/oauth2/token", {"grant_type": "refresh_token", "client_id": "limits-test", "refresh_token": "ory_rt_made-up"}, {}),
    "registration": ("POST", "/register", json.dumps({"client_name": "x", "redirect_uris": ["https://evil.example.com/cb"]}).encode(), {"Content-Type": "application/json"}),
}
SPOOF = {"X-Forwarded-For": "198.51.100.9", "X-Real-IP": "198.51.100.9", "Forwarded": "for=198.51.100.9", "X-Ambit-Client-Address": "198.51.100.9"}
GW_ANSWER = {}  # the first 429 of each bucket: which layer answered


def send(addr, bucket, port=None, extra=None):
    """One request of a bucket from the made-up address addr. Returns the HTTP status."""
    global CLIENT_ADDR
    CLIENT_ADDR = addr
    ROUTES[(AUTH, 443)] = port or PORTS[0]
    m, path, data, hd = SHAPES[bucket]
    st, h, body = Browser().req(m, base_url(path), data, {**hd, **(extra or {})})
    if st == 429 and bucket not in GW_ANSWER:
        GW_ANSWER[bucket] = "ambit-auth" if h.get("Retry-After") else "the Gateway"
    return st


def mcp_no_credential(addr, port=None, token=None):
    global CLIENT_ADDR
    CLIENT_ADDR = addr
    ROUTES[(MCP, 443)] = port or PORTS[0]
    hd = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if token: hd["Authorization"] = "Bearer " + token
    return Browser().req("POST", f"https://{MCP}/mcp", json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}).encode(), hd)[0]


def mcp_status(addr, key=None, token=None):
    """One MCP initialize with a key or a token from addr: 200, or 429 when limited (an MCP request over a limit may also answer 200 with a
    JSON-RPC error that says 'rate limit': that is reported as 429)."""
    global CLIENT_ADDR
    CLIENT_ADDR = addr
    ROUTES[(MCP, 443)] = PORTS[0]
    hd = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if key: hd["x-api-key"] = key
    if token: hd["Authorization"] = "Bearer " + token
    st, h, body = Browser().req("POST", f"https://{MCP}/mcp", json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "limits", "version": "1"}}}).encode(), hd)
    return 429 if st == 200 and "rate limit" in body.lower() else st


INIT = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "limits", "version": "1"}}}).encode()


def mcp_raw(addr, headers):
    """One MCP initialize from addr with exactly these extra headers (a header may name a key, a token, or junk). 429 when limited (also the 200 with a rate limit error)."""
    global CLIENT_ADDR
    CLIENT_ADDR = addr
    ROUTES[(MCP, 443)] = PORTS[0]
    hd = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream", **headers}
    st, h, body = Browser().req("POST", f"https://{MCP}/mcp", INIT, hd)
    return 429 if st == 200 and "rate limit" in body.lower() else st


JUNK_HEADER = ["eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9"]  # replaced by the header of a real token (with its kid): the gateway then really checks the signature


def junk_jwt(i):
    """A token in the shape of a JWT (a header that starts with eyJ, a payload, a signature) that no key signed."""
    return JUNK_HEADER[0] + "." + b64(json.dumps({"sub": f"junk{i}", "iss": ISS, "aud": [MCP_URL]}).encode()) + "." + b64(os.urandom(256))


def mcp_dup(addr, values):
    """One MCP initialize from addr with the Authorization header repeated on the wire (once for each value): the gateway sees a list."""
    global CLIENT_ADDR
    CLIENT_ADDR = addr
    ROUTES[(MCP, 443)] = PORTS[0]
    c = Conn(MCP, 443, timeout=20)
    try:
        c.putrequest("POST", "/mcp", skip_host=False)
        for k, v in (("Content-Type", "application/json"), ("Accept", "application/json, text/event-stream"), ("Content-Length", str(len(INIT)))):
            c.putheader(k, v)
        for v in values: c.putheader("Authorization", v)
        c.endheaders(INIT)
        r = c.getresponse(); body = r.read().decode("utf-8", "replace")
        return 429 if r.status == 200 and "rate limit" in body.lower() else r.status
    finally:
        c.close()


def start_of_window(unit="minute"):
    """The limits count in fixed windows of the clock: start a burst early in one."""
    t = time.gmtime()
    if unit == "minute" and t.tm_sec > 20: time.sleep(62 - t.tm_sec)
    if unit == "hour" and t.tm_min >= 56: time.sleep(3700 - t.tm_min * 60 - t.tm_sec)


def minute_now(): return int(time.time() // 60)


def burst(fn, n, unit="minute"):
    """n calls of fn(i) in one window of the clock; returns the statuses. Run again when a window ended in the middle."""
    for _ in range(3):
        start_of_window(unit)
        w0 = minute_now() if unit == "minute" else int(time.time() // 3600)
        codes = [fn(i) for i in range(n)]
        w1 = minute_now() if unit == "minute" else int(time.time() // 3600)
        if w0 == w1: return codes
    raise RuntimeError("a burst always crossed a window")


def allowed(codes): return sum(1 for c in codes if c != 429)


if MODE == "buckets":
    names = ["discovery", "signin", "token", "registration"]
    for i, b in enumerate(names):
        addr = f"203.0.113.{10 + i}"
        n = LIM[b] + 8
        codes = burst(lambda _: send(addr, b), n, "hour" if b == "registration" else "minute")
        a = allowed(codes)
        check(f"19 {b} bucket: {n} requests from one address, {a} passed and the rest got 429 (limit {LIM[b]})", a == LIM[b] and codes[LIM[b]:] == [429] * 8 and set(codes[:LIM[b]]) <= OPEN, codes)
        others = {o: send(addr, o) for o in names if o != b}
        check(f"19 {b} bucket used up: the other three buckets of the same address are open", all(c != 429 for c in others.values()), others)
    print("   the first 429 of each bucket came from: " + ", ".join(f"{k}: {v}" for k, v in GW_ANSWER.items()), flush=True)
    check("19 every bucket is stopped by the Gateway (the shared counter of the ratelimit service), not only by ambit-auth", set(GW_ANSWER.values()) == {"the Gateway"}, GW_ANSWER)

    # the 401 of a call without a credential is in the discovery bucket; a call with a credential is not
    addr = "203.0.113.40"
    codes = burst(lambda _: mcp_no_credential(addr), LIM["discovery"] + 5)
    check(f"19 the MCP URL without a credential: 401 {allowed(codes)} times, then 429 (the discovery bucket, limit {LIM['discovery']})", allowed(codes) == LIM["discovery"] and set(codes[:LIM["discovery"]]) == {401} and codes[LIM["discovery"]:] == [429] * 5, sorted(set(codes)))
    check("19 ... and a call with a (wrong) token from the same address is not limited: it is in the mcp bucket", mcp_no_credential(addr, token="not-a-token") == 401)
    check("19 ... nor are the sign-in pages of that address", send(addr, "signin") != 429)

    # an address in a header changes nothing
    A = "203.0.113.55"  # used up and then written to in one window (the bursts above may have taken longer than a minute)
    n = LIM["signin"] + 2
    mixed = burst(lambda i: send(A, "signin") if i < n else send(A, "signin", extra={**SPOOF, "X-Forwarded-For": f"198.51.100.{i}"}), n + 6)
    spoofed = mixed[n:]
    check("19 spoofed X-Forwarded-For, X-Real-IP, Forwarded and X-Ambit-Client-Address do not free a limited address", allowed(mixed) == LIM["signin"] and set(spoofed) == {429}, mixed)
    B = "203.0.113.50"
    claims = burst(lambda i: send(B, "signin", extra={"X-Forwarded-For": A, "X-Real-IP": A, "Forwarded": f"for={A}", "X-Ambit-Client-Address": A}), LIM["signin"] + 6)
    check(f"19 another address that writes the limited address into those headers is counted as itself: {allowed(claims)} passed (limit {LIM['signin']}), not 0", allowed(claims) == LIM["signin"], claims)
    C = "203.0.113.51"
    fresh = burst(lambda i: send(C, "signin", extra={"X-Forwarded-For": f"198.51.100.{i % 250}", "X-Ambit-Client-Address": f"198.51.100.{i % 250}"}), LIM["signin"] + 6)
    check(f"19 a new made-up address in a header on every request gives no extra budget: {allowed(fresh)} passed (limit {LIM['signin']})", allowed(fresh) == LIM["signin"], fresh)

    # two Gateway pods count together
    D = "203.0.113.52"
    # the token bucket: only the Gateway counts it (ambit-auth does not see /oauth2/token), so shared counters are the Gateway's
    two = burst(lambda i: send(D, "token", port=PORTS[i % 2]), LIM["token"] + 12)
    check(f"19 two Gateway pods count together: {len(two)} token requests alternating between them, {allowed(two)} passed (limit {LIM['token']}, not {2 * LIM['token']})", allowed(two) == LIM["token"] and set(two[:LIM["token"]]) <= OPEN, two)
    both = [send(D, "token", port=p) for p in PORTS]
    check("19 ... and the address is limited on both pods", both == [429, 429], both)
    # path spellings that a router may read as /oauth2/token are never a way around the token bucket: if one reaches Hydra it is counted as a token request
    E = "203.0.113.53"
    def alias(i):
        global CLIENT_ADDR
        CLIENT_ADDR = E; ROUTES[(AUTH, 443)] = PORTS[0]
        return Browser().req("POST", f"https://{AUTH}/oauth2/%74oken", urllib.parse.urlencode({"grant_type": "refresh_token", "client_id": "limits-test", "refresh_token": "x"}).encode(), {"Content-Type": "application/x-www-form-urlencoded"})[0]
    al = burst(alias, LIM["token"] + 10)
    check("19 a percent-encoded spelling of /oauth2/token is either not served, or counted in the token bucket (no way around the limit)", not any(c in (400, 401) for c in al) or 429 in al[: LIM["token"] + 2], al)
    # the fifth bucket: calls with a key or token (mcp), and the count per credential (an agent), not one counter for every token
    if LIM_MCP:
        F = "203.0.113.54"
        calls = burst(lambda i: mcp_status(F, key=os.environ["KEY"]), LIM_MCP + 8)
        check(f"19 mcp bucket: {len(calls)} calls with a valid key from one address, {allowed(calls)} passed (limit {LIM_MCP})", allowed(calls) == LIM_MCP and set(calls[:LIM_MCP]) <= {200}, calls)
    if LIM_CRED:
        CLIENT_ADDR = "203.0.115.1"  # a fresh address for the registrations and sign-ins
        ROUTES[(AUTH, 443)] = PORTS[0]
        _, tA = connect("person150", ("time",)); _, tB = connect("person151", ("time",))
        toks = [tA["access_token"], tB["access_token"]]
        # both agents in ONE window of the clock, interleaved: a counter that they shared would stop each at half
        calls = burst(lambda i: mcp_status(f"203.0.114.{(i % 250) + 1}", token=toks[i % 2]), 2 * (LIM_CRED + 8))  # a new source address for (nearly) every call: only the credential count can stop it
        res = [calls[0::2], calls[1::2]]
        check(f"19 per credential: a token's calls stop at the limit of {LIM_CRED} a minute for its agent, and another agent's token has its own count ({allowed(res[0])} and {allowed(res[1])} passed)",
              all(allowed(c) == LIM_CRED for c in res), [sorted(set(c)) for c in res])
    # #436: the bearer bucket
    if LIM_BEARER and LIM_CRED and LIM_MCP:
        check(f"19 bearer bucket: its limit ({LIM_BEARER} a minute) is above the key limit ({LIM_MCP}) in this test", LIM_BEARER > LIM_MCP)
        CLIENT_ADDR = "203.0.115.2"; ROUTES[(AUTH, 443)] = PORTS[0]
        _, tC = connect("person152", ("time",)); _, tD = connect("person153", ("time",))
        tk = [tC["access_token"], tD["access_token"]]
        JUNK_HEADER[0] = tk[0].split(".")[0]  # the junk carries a real header (its key ID): the Gateway finds the key and checks the signature, as for a real token
        KEY = os.environ["KEY"]
        G = "203.0.113.70"
        nb, nk = LIM_BEARER + 6, LIM_MCP + 6
        # one window of the clock: bearer calls, then key calls, then bearer calls again, all from the address G
        plan = [("t", i) for i in range(nb)] + [("k", i) for i in range(nk)] + [("t", nb + i) for i in range(4)]
        res = burst(lambda i: mcp_status(G, token=tk[plan[i][1] % 2]) if plan[i][0] == "t" else mcp_status(G, key=KEY), len(plan))
        tcodes = [c for (kind, _), c in zip(plan, res) if kind == "t"]
        kcodes = [c for (kind, _), c in zip(plan, res) if kind == "k"]
        check(f"19 bearer bucket: {len(tcodes)} calls with valid tokens (of two agents) from one address, {allowed(tcodes)} passed (limit {LIM_BEARER}), then 429", allowed(tcodes) == LIM_BEARER and set(tcodes[:LIM_BEARER]) == {200}, tcodes)
        check(f"19 ... and keys from the same address, in the same minute, stop at their own limit: {allowed(kcodes)} of {len(kcodes)} passed (limit {LIM_MCP}), not 0 (the bearer bucket is used up) and not {LIM_BEARER}", allowed(kcodes) == LIM_MCP and set(kcodes[:LIM_MCP]) == {200}, kcodes)
        # a flood of junk in the shape of a JWT from one address: it stops at the same limit, and the key limit of that address is not used up by it
        H = "203.0.113.71"
        nj, nk2 = LIM_BEARER + 8, LIM_MCP + 6
        res = burst(lambda i: mcp_raw(H, {"Authorization": "Bearer " + junk_jwt(i)}) if i < nj else mcp_raw(H, {"x-api-key": KEY}), nj + nk2)
        junk, keys2 = res[:nj], res[nj:]
        check(f"19 junk in the shape of a JWT: {allowed(junk)} of {nj} calls from one address got the 401 of the token check, then 429 (limit {LIM_BEARER}, the same as for valid tokens)", allowed(junk) == LIM_BEARER and set(junk[:LIM_BEARER]) == {401} and junk[LIM_BEARER:] == [429] * 8, sorted(set(junk)))
        check(f"19 ... and it did not use up the key bucket of that address: {allowed(keys2)} of {nk2} key calls passed (limit {LIM_MCP})", allowed(keys2) == LIM_MCP and set(keys2[:LIM_MCP]) == {200}, keys2)
        check("19 ... and valid tokens from another address are not affected", mcp_status("203.0.113.72", token=tk[0]) == 200)
        # what is not in the shape stays in the key bucket: it must never get the higher limit
        cases = {
            "Bearer Bearer <key> (the key rule reads every 'Bearer ' off the header: this is a key)": ("203.0.113.73", {"Authorization": "Bearer Bearer " + KEY}),
            "Bearer <key>": ("203.0.113.74", {"Authorization": "Bearer " + KEY}),
            "Bearer abc (not in the shape of a JWT)": ("203.0.113.75", {"Authorization": "Bearer abc"}),
            # (a wrong key: the valid key of this test has its own count per minute, which the key calls above have used)
            "a key header and a token together": ("203.0.113.76", {"x-api-key": "ambit_" + "x" * 40, "Authorization": "Bearer " + junk_jwt(0)}),
            "a token with a space and a key behind it": ("203.0.113.77", {"Authorization": "Bearer " + junk_jwt(1) + " Bearer " + KEY}),
        }
        for name, (addr, hd) in cases.items():
            codes = burst(lambda i: mcp_raw(addr, hd), LIM_MCP + 6)
            check(f"19 {name}: counts in the key bucket: {allowed(codes)} of {len(codes)} passed (limit {LIM_MCP}, not the bearer limit {LIM_BEARER})", allowed(codes) == LIM_MCP and codes[LIM_MCP:] == [429] * 6, sorted(set(codes)))
            print(f"   ({name}: the first answer was {codes[0]})", flush=True)
        # the scheme is read in any letter case (as the Gateway reads it): a platform that writes "bearer" gets the bearer bucket too
        L = "203.0.113.78"
        low = burst(lambda i: mcp_raw(L, {"Authorization": "bearer " + junk_jwt(i)}), LIM_BEARER + 6)
        check(f"19 'bearer' in small letters is the same bucket as 'Bearer': {allowed(low)} of {len(low)} passed (limit {LIM_BEARER})", allowed(low) == LIM_BEARER and set(low[:LIM_BEARER]) == {401}, sorted(set(low)))
        # a repeated Authorization header is a list for the Gateway (a method on it would be an error): it stays in the key bucket, and it is counted (not uncounted)
        D = "203.0.113.79"
        dup = burst(lambda i: mcp_dup(D, ["Bearer " + junk_jwt(i), "Bearer " + junk_jwt(i + 1000)]), LIM_MCP + 6)
        check(f"19 two Authorization headers in one request: counts in the key bucket: {allowed(dup)} of {len(dup)} passed (limit {LIM_MCP}, not the bearer limit {LIM_BEARER}), and none is uncounted", allowed(dup) == LIM_MCP and dup[LIM_MCP:] == [429] * 6, sorted(set(dup)))
        print(f"   (two Authorization headers: the first answer was {dup[0]})", flush=True)
        # a very long header in the shape of a JWT stays in the key bucket (it is not parsed at the higher limit)
        big = "Bearer " + JUNK_HEADER[0] + "." + "A" * 9000 + ".c2ln"
        M = "203.0.113.80"
        huge = burst(lambda i: mcp_raw(M, {"Authorization": big}), LIM_MCP + 6)
        check(f"19 a header of more than 8192 characters counts in the key bucket: {allowed(huge)} of {len(huge)} passed (limit {LIM_MCP})", allowed(huge) == LIM_MCP and huge[LIM_MCP:] == [429] * 6, sorted(set(huge)))
    # N8: the bucket is chosen by the host too. Sign-in paths on the MCP host (a 404 there) stay in the mcp bucket: a flood there neither spreads over the
    # sign-in and token buckets of the same address, nor is counted as sign-in traffic.
    if LIM_MCP:
        H = "203.0.113.56"
        def on_mcp_host(i):
            global CLIENT_ADDR
            CLIENT_ADDR = H; ROUTES[(MCP, 443)] = PORTS[0]
            path = ["/oauth2/token", "/login", "/register"][i % 3]
            return Browser().req("POST" if path in ("/oauth2/token", "/register") else "GET", f"https://{MCP}{path}", b"x=y" if path in ("/oauth2/token", "/register") else None, {"Content-Type": "application/x-www-form-urlencoded"})[0]
        flood = burst(on_mcp_host, LIM_MCP + 10)
        check(f"19 sign-in paths on the MCP host: {allowed(flood)} of {len(flood)} passed (the mcp limit {LIM_MCP}), then 429: they count in the mcp bucket", allowed(flood) == LIM_MCP and set(flood[:LIM_MCP]) == {404}, sorted(set(flood)))
        after = {b: send(H, b) for b in ("discovery", "signin", "token")}
        check("19 ... and the sign-in host's discovery, sign-in and token buckets of the same address are still open (no spreading)", all(c != 429 for c in after.values()), after)
        # a host name in other letters is the same host: it must not be a way to another bucket
        U = "203.0.113.57"
        def upper(i):
            global CLIENT_ADDR
            CLIENT_ADDR = U; ROUTES[(AUTH, 443)] = PORTS[0]
            return Browser().req("GET", base_url("/error"), None, {"Host": AUTH.upper()})[0]
        up = burst(upper, LIM["signin"] + 6)
        served = [c for c in up if c not in (404, 429)]
        check(f"19 the sign-in host written in capital letters is either not served (404: the route matches the host name exactly) or counted as the sign-in host: {len(served)} of {len(up)} reached a page, {allowed(up)} passed", not served or allowed(up) == LIM["signin"], sorted(set(up)))
    # N2: IPv6 registrations count by the /48 in ambit-auth: many /64 networks of one /48 share the 10 (here 5) an hour; another /48 has its own
    def reg6(net, i):
        global CLIENT_ADDR
        CLIENT_ADDR = f"2001:db8:{net}:{i:x}::1"; ROUTES[(AUTH, 443)] = PORTS[0]
        st, h, body = Browser().req("POST", base_url("/register"), SHAPES["registration"][2], SHAPES["registration"][3])
        if st == 429: GW_ANSWER["reg6"] = "ambit-auth" if h.get("Retry-After") else "the Gateway"
        return st
    six = burst(lambda i: reg6(0x48, i + 1), LIM["registration"] + 7, "hour")
    check(f"19 IPv6: {len(six)} registrations from {len(six)} different /64 networks of one /48: {allowed(six)} passed (limit {LIM['registration']}), then 429", allowed(six) == LIM["registration"] and six[LIM["registration"]:] == [429] * 7, six)
    check("19 ... and the 429 is ambit-auth's (the Gateway counts the single addresses, and each had one request)", GW_ANSWER.get("reg6") == "ambit-auth", GW_ANSWER)
    check("19 ... another /48 registers", reg6(0x49, 1) != 429)
    # N2: the global limit of accepted registrations, last (it uses the quota that the sign-ins above need)
    if LIM_GLOBAL:
        got = []
        for i in range(LIM_GLOBAL + 6):
            CLIENT_ADDR = f"203.0.116.{i + 1}"; ROUTES[(AUTH, 443)] = PORTS[0]
            st, h, body = Browser().req("POST", base_url("/register"), json.dumps({"client_name": "g", "redirect_uris": [REDIRECT]}).encode(), {"Content-Type": "application/json"})
            got.append((st, h.get("Retry-After")))
        codes = [c for c, _ in got]
        first429 = codes.index(429) if 429 in codes else -1
        check(f"19 global registration limit: registrations from {len(got)} different addresses: {codes.count(201)} created (limit {LIM_GLOBAL} an hour for everybody), then 429 with Retry-After 3600", 1 <= codes.count(201) <= LIM_GLOBAL and first429 > 0 and all(c == 429 for c in codes[first429:]) and got[first429][1] == "3600", got)
    # ambit-auth's second line: its own counter in Valkey, shared by its replicas, with the same numbers (seen in its metrics by the shell script)

elif MODE == "second":
    # The ratelimit service is down (FailOpen): the Gateway lets requests through uncounted, and ambit-auth's own counter (Valkey, shared by its replicas,
    # the same numbers) is the second line.
    addr = "203.0.113.60"
    codes = burst(lambda i: send(addr, "signin", port=PORTS[i % 2]), LIM["signin"] + 6)
    check(f"19 ratelimit service down: ambit-auth stops the sign-in pages at the same limit ({allowed(codes)} passed of {len(codes)}, limit {LIM['signin']})", allowed(codes) == LIM["signin"], codes)
    check("19 ... and it is ambit-auth that answers 429 (it sends Retry-After), not the Gateway", GW_ANSWER.get("signin") == "ambit-auth", GW_ANSWER)
    check("19 ... the discovery bucket of the same address is its own", send(addr, "discovery") == 200)

elif MODE == "nat":
    # One company address, many people. A platform registers once (one client for all of its users), then each person signs in. All from one address.
    # The people sign in at the same time (WORKERS at once): that is what an announcement does, and a sign-in takes seconds on a small cluster.
    import threading
    WORKERS = 4
    COUNT = {"signin": 0, "token": 0, "discovery": 0, "mcp": 0}
    lock = threading.Lock()
    orig = Browser.req

    def counted(self, method, url, data=None, headers=None):
        p = urllib.parse.urlparse(url)
        if p.hostname == AUTH:
            with lock:
                if p.path in ("/oauth2/auth", "/login", "/callback", "/consent", "/error"): COUNT["signin"] += 1
                elif p.path in ("/oauth2/token", "/oauth2/revoke"): COUNT["token"] += 1
                elif p.path.startswith("/.well-known/"): COUNT["discovery"] += 1
        elif p.hostname == MCP:
            with lock: COUNT["mcp"] += 1
        return orig(self, method, url, data, headers)
    Browser.req = counted

    def one_signin(cid, who, deny=False):
        """A whole sign-in as a person. deny: the person ends it with Deny at the consent page (the same requests, 7 in the sign-in bucket, but no agent is
        made: the console of a small test cluster makes about one agent a second, and that would be the limit of this test, not the Gateway's)."""
        b = Browser()
        if deny:
            kind, det = sign_in(b, cid, f"person{who}", consent=False)
            if kind != "consent":
                return ("page", det[0], det[2], det[1][:160]) if kind == "page" else (kind, det)
            st, body, url, v, state = det
            form = dict(re.findall(r'name="(challenge|csrf)" value="([^"]*)"', body)); form["action"] = "deny"
            st, h, t = b.req("POST", base_url("/consent"), urllib.parse.urlencode(list(form.items())).encode(), {"Origin": ISS, "Content-Type": "application/x-www-form-urlencoded"})
            if st != 302:
                return ("page", st, "/consent", t[:160])
            st, h, t, url = b.follow(urllib.parse.urljoin(base_url("/consent"), h["Location"]))
            if not url.startswith(REDIRECT + "?") or "error=access_denied" not in url:
                return ("page", st, url, t[:160])
            return None
        kind, res = sign_in(b, cid, f"person{who}", tick=("time",))
        if kind != "code":
            return ("page", res[0], res[2], res[1][:160]) if kind == "page" else (kind, res)
        st, tok = exchange(cid, res)
        return None if st == 200 else ("token", st)

    # 1. what one sign-in costs: alone, from an address of its own
    CLIENT_ADDR = "203.0.113.99"
    cid0 = register(REDIRECT, "NAT cost")[2].get("client_id")
    for k in COUNT: COUNT[k] = 0
    bad = one_signin(cid0, 190)  # a completed sign-in (an agent is made)
    cost = dict(COUNT)
    check(f"19 NAT: one whole sign-in alone costs {cost['signin']} requests in the sign-in bucket and {cost['token']} in the token bucket", bad is None and cost["signin"] >= 6, (bad, cost))
    typical = max(cost["signin"], 1)
    # 2. many at once from one address, until the limit answers 429
    result = None
    for attempt in range(3):
        addr = f"203.0.113.{100 + attempt}"
        start_of_window("minute")
        CLIENT_ADDR = addr
        t0, w0 = time.time(), minute_now()
        cid = register(REDIRECT, "NAT test")[2].get("client_id")
        state = {"ok": 0, "next": 0, "fail": None}

        def worker():
            while state["fail"] is None and time.time() - t0 < 58:
                with lock:
                    who = 100 + state["next"]; state["next"] += 1
                bad = one_signin(cid, who % 190, deny=True)
                with lock:
                    if bad is None: state["ok"] += 1
                    elif state["fail"] is None: state["fail"] = bad
        ts = [threading.Thread(target=worker) for _ in range(WORKERS)]
        [t.start() for t in ts]; [t.join() for t in ts]
        elapsed = time.time() - t0
        if minute_now() == w0 and state["fail"] and state["fail"][:2] == ("page", 429):
            result = (state["ok"], elapsed, addr); break
        print(f"   attempt {attempt + 1} not valid (window crossed or no 429: {state['fail']}, {state['ok']} sign-ins in {elapsed:.0f} s)", flush=True)
    check("19 NAT: one address reaches the sign-in limit within one minute and is then answered 429", result is not None)
    ok, elapsed, addr = result
    expect = LIM["signin"] // typical
    print(f"   NAT: one shared address made {ok} sign-ins (each ended by the person with Deny: the same 7 requests as a completed one) in {elapsed:.0f} s ({WORKERS} at a time) before the sign-in limit ({LIM['signin']} a minute) answered 429; "
          f"a sign-in costs {typical} requests in the sign-in bucket and {cost['token']} in the token bucket, so the limit is {expect} sign-ins", flush=True)
    check(f"19 NAT: {ok} sign-ins from one address in a minute fit the default sign-in limit of {LIM['signin']} ({typical} requests each; the {WORKERS} that were under way when it answered 429 did not finish)",
          expect - WORKERS <= ok <= expect, (ok, expect))
    other = "203.0.113.150"
    CLIENT_ADDR = other
    kind, res = sign_in(Browser(), cid, "person199", tick=("time",))
    check("19 NAT: another address signs in while the first one is limited", kind == "code", (kind, res if kind != "code" else ""))

    # ---- #436: the calls of a hosted platform with tokens, from one address (the bearer bucket)
    # What one interaction of a person costs: initialize, notifications/initialized, tools/list, three tool calls, and the end of the session: 7 requests.
    def interaction(token, k=3):
        """One interaction of a person with the platform's MCP client; returns the statuses of its requests (429 for a request that the limit refused)."""
        out = []
        def post(body, sid=None):
            st, hd, data, t = mcp_post(PORTS[0], body, token, None, sid)
            return (429 if st == 200 and "rate limit" in t.lower() else st), hd
        st, hd = post({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "nat", "version": "1"}}}); out.append(st)
        if st != 200: return out
        sid = hd.get("mcp-session-id")
        out.append(post({"jsonrpc": "2.0", "method": "notifications/initialized"}, sid)[0])
        out.append(post({"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, sid)[0])
        for j in range(k):
            out.append(post({"jsonrpc": "2.0", "id": 3 + j, "method": "tools/call", "params": {"name": TOOL, "arguments": {"timezone": "UTC"}}}, sid)[0])
        ROUTES[(MCP, 443)] = PORTS[0]
        st, h, t = Browser().req("DELETE", MCP_URL, None, {"Authorization": "Bearer " + token, "Mcp-Session-Id": sid or ""})
        out.append(429 if st == 429 else st)
        return out
    CLIENT_ADDR = "203.0.118.1"; ROUTES[(AUTH, 443)] = PORTS[0]
    toks = [connect(f"person16{i}", ("time",))[1]["access_token"] for i in range(3)]
    JUNK_HEADER[0] = toks[0].split(".")[0]
    CLIENT_ADDR = "203.0.118.2"
    for k in COUNT: COUNT[k] = 0
    codes = interaction(toks[0])
    mcp_cost = COUNT["mcp"]
    check(f"19 bearer bucket: one interaction of a person (initialize, initialized, tools/list, 3 tool calls, end of the session) costs {mcp_cost} requests", mcp_cost == 7 and all(c in (200, 202, 204) for c in codes), (mcp_cost, codes))
    # many people at once from one address, until the bearer limit answers 429 (three agents, so that the count per token, 600 a minute, is not the limit)
    BEARER = LIM_BEARER or 1200
    result = None
    for attempt in range(3):
        addr = f"203.0.118.{10 + attempt}"
        start_of_window("minute")
        CLIENT_ADDR = addr
        t0, w0 = time.time(), minute_now()
        st = {"allowed": 0, "limited": 0, "requests": 0, "inter": 0, "n": 0, "stop": False, "bad": []}
        def mworker():
            while not st["stop"] and time.time() - t0 < 56:
                with lock:
                    tok = toks[st["n"] % 3]; st["n"] += 1
                codes = interaction(tok)
                with lock:
                    st["inter"] += 1
                    for c in codes:
                        st["requests"] += 1
                        if c == 429: st["limited"] += 1; st["stop"] = True
                        elif c in (200, 202, 204): st["allowed"] += 1
                        else: st["bad"].append(c); st["stop"] = True  # a 401, 403 or 5xx is not a successful call: the measurement would be wrong
        ts = [threading.Thread(target=mworker) for _ in range(WORKERS)]
        [t.start() for t in ts]; [t.join() for t in ts]
        elapsed = time.time() - t0
        if minute_now() == w0 and st["limited"] and not st["bad"]:
            result = (dict(st), elapsed); break
        print(f"   attempt {attempt + 1} not valid (window crossed or no 429: {st['allowed']} allowed in {elapsed:.0f} s)", flush=True)
    check("19 bearer bucket: one address reaches the default limit within one minute and is then answered 429", result is not None)
    st, elapsed = result
    print(f"   MCP: one address with tokens of three agents sent {st['requests']} requests ({st['inter']} interactions of {mcp_cost}) in {elapsed:.0f} s, {WORKERS} at a time; {st['allowed']} passed and the bearer limit "
          f"({BEARER} a minute) answered 429 for the rest, which is {st['allowed'] // max(mcp_cost, 1)} whole interactions a minute from one address", flush=True)
    check(f"19 bearer bucket: exactly {BEARER} requests of one address passed in the minute (the default limit), every one with a success status (200, 202 or 204); the rest got 429", st["allowed"] == BEARER and not st["bad"], st)
    # what a junk token costs the gateway, next to a valid call: the median time of 40 each, one address each (so that no limit is in the way)
    import statistics
    def timed(fn, n=40):
        out = []
        for i in range(n):
            t = time.time(); r = fn(i); out.append((time.time() - t) * 1000)
        return statistics.median(out), r
    junk_ms, jr = timed(lambda i: mcp_raw(f"203.0.119.{i + 1}", {"Authorization": "Bearer " + junk_jwt(i)}))
    valid_ms, vr = timed(lambda i: mcp_raw(f"203.0.120.{i + 1}", {"Authorization": "Bearer " + toks[i % 3]}))
    print(f"   cost of one request through the Gateway (median of 40, measured by the client, one address each): a token in the shape of a JWT that no key signed {junk_ms:.0f} ms (HTTP {jr}); a valid token {valid_ms:.0f} ms (HTTP {vr})", flush=True)
    check("19 bearer bucket: a junk token (401) is not slower than a valid call: the 401 path is the cheap one", jr == 401 and vr == 200 and junk_ms <= valid_ms * 1.5 + 5, (junk_ms, valid_ms))

else:
    sys.exit("unknown mode " + MODE)

print()
print("FAILED: " + ", ".join(fails) if fails else "all checks passed")
sys.exit(1 if fails else 0)
