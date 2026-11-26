#!/usr/bin/env python3
"""End-to-end checks of the emergency rotation of the signing keys (test/stepoauth.sh check 15, after gateway.py). Standard library only.

What is checked, with the names of the design (R: rotation, E: emergency, J: JWKS failures) as far as they apply to an emergency rotation:
J1 a restart of the gateway controller loses no call; J2 with Hydra's key endpoint down for longer than the gateway's cache (5 minutes) the
tokens that were issued keep working; J3 and a restart of the controller in that time changes nothing; E2 an emergency rotation while Hydra is down
fails, says so, and has already ended every older grant; R1 the rotation replaces both signing sets by one new key; R2 the published keys
are the new key only and Hydra's pods stay ready (one key per set); R3 a new connection gets a token with the new key ID; R4 the refresh of
an old grant is refused; R5 the check on every gateway pod (an old-key probe token is 401, a new-key probe token is 403), with the time;
R6 after the block is lifted new tokens work on every pod and old tokens are refused; R7 a new grant refreshes with the new key ID;
E1 the block holds all the time and is independent of the rotation.

Environment: as gateway.py, plus STOPCTL and ADMIN_PW (test/oauthtest/stopctl.sh)."""
import os
src = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "gateway.py")).read().split("# 1 · no token")[0]
exec(src)

CTL = os.environ["STOPCTL"]
POL = "agentgatewaypolicy/ambit-mcp-public-oauth"


def ctl(*a):
    return subprocess.run([CTL, *a], capture_output=True, text=True, timeout=300).stdout.strip()


def ctl_json(*a):
    """(HTTP status, JSON body or {}) of a console admin call made by stopctl.sh."""
    out = ctl(*a).split("\n", 1)
    body = {}
    try: body = json.loads(out[1]) if len(out) > 1 else {}
    except Exception: pass
    return out[0].strip(), body


def header(tok):
    p = tok.split(".")[0]
    return json.loads(base64.urlsafe_b64decode(p + "=" * (-len(p) % 4)))


def jwks_kids():
    at(PORTS[0])
    st, hd, body = Browser().req("GET", f"https://{AUTH}/.well-known/jwks.json")
    try: return sorted(k["kid"] for k in json.loads(body)["keys"])
    except Exception: return None


def connect_wait(*a):
    end = time.time() + 120
    while True:
        try: return connect(*a)
        except RuntimeError as e:
            if "503" not in str(e) or time.time() > end: raise
            time.sleep(3)


def code_of(port, tok):
    return mcp_post(port, {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}, token=tok)[0]


def block_on():
    kube("patch", POL, "--type", "json", "-p", '[{"op":"add","path":"/spec/traffic/authorization/policy/matchExpressions/-","value":"false"}]')


def block_off():
    n = len(json.loads(kube("get", POL, "-o", "json"))["spec"]["traffic"]["authorization"]["policy"]["matchExpressions"])
    kube("patch", POL, "--type", "json", "-p", '[{"op":"remove","path":"/spec/traffic/authorization/policy/matchExpressions/%d"}]' % (n - 1))


def all_pods(fn):
    return all(fn(p) for p in PORTS)


def controller():
    return kube("get", "deploy", "-l", "app.kubernetes.io/name=agentgateway", "-o", "name").split()[0]


def hydra_down():
    kube("scale", "statefulset/ambit-hydra", "--replicas=0")
    wait(lambda: kube("get", "pods", "-l", "app.kubernetes.io/name=ambit-hydra", "-o", "name") == "", 120, 2)


def hydra_up():
    kube("scale", "statefulset/ambit-hydra", "--replicas=2")
    kube("rollout", "status", "statefulset/ambit-hydra", "--timeout=300s")


# ---------------------------------------------------------------------------------------------------------------------
# R0 · a connection with the key that exists now
cidA, tokA = connect_wait("alice", ("time",), "BeforeRotation")
kidA = header(tokA["access_token"]).get("kid")
code, st0 = ctl_json("keys")
all_kids = sorted(st0.get("published", []))
check("R0 the status shows the published keys (public key list only): the key of the token, and one key per signing set", code == "200" and len(all_kids) == 2 and kidA in all_kids and jwks_kids() == all_kids and "sets" not in st0, (code, st0, kidA, jwks_kids()))
check("R0 the token works on every pod", accepted_everywhere(tokA["access_token"], 120))
# a second connection, only for R4 (the refresh of a grant made before a rotation), so that R4 does not depend on E2
cidC, tokC = connect_wait("alice", ("time",), "BeforeRotationToo")

# J1 · a restart of the gateway controller loses no call
kube("rollout", "restart", controller())
bad, t = 0, time.time()
while time.time() - t < 45:
    bad += sum(1 for p in PORTS if tools(p, tokA["access_token"])[0] != 200)
    time.sleep(0.5)
kube("rollout", "status", controller(), "--timeout=300s")
check("J1 a restart of the controller: no call with a token failed", bad == 0, bad)

# J2, J3 · the key endpoint down for longer than the gateway's cache (5 minutes): issued tokens keep working, also after a controller restart
st, j = token(cidA, grant_type="refresh_token", refresh_token=tokA["refresh_token"])
check("J2 a refresh before Hydra goes down", st == 200 and j.get("access_token"), st)
tokA = j
hydra_down()
t_down = time.time()
mid = False
while time.time() - t_down < 335:
    if time.time() - t_down > 120 and not mid:
        mid = all_pods(lambda p: tools(p, tokA["access_token"])[0] == 200)
    time.sleep(10)
check("J2 Hydra's key endpoint down for 5 minutes and more: the issued token still works on every pod (the last keys are kept)", mid and all_pods(lambda p: tools(p, tokA["access_token"])[0] == 200))
kube("rollout", "restart", controller())
kube("rollout", "status", controller(), "--timeout=300s")
check("J3 a restart of the controller while the key endpoint is down: the token still works", wait(lambda: all_pods(lambda p: tools(p, tokA["access_token"])[0] == 200), 120, 2))

# E2 · the rotation while Hydra is down: it fails, says to run it again, and has already ended every older grant
block_on()
check("E1 the block is in force on every pod before anything is rotated: a token that worked is refused", wait(lambda: all_pods(lambda p: tools(p, tokA["access_token"])[0] in (401, 403)), 30, 1))
code, body = ctl_json("rotate")
check("E2 a rotation while Hydra is down fails (502) and says to run it again", code == "502" and "run it again" in json.dumps(body), (code, body))
check("E2 ... the time of the rotation is written first (the token hook refuses every older grant)", "since" in kube("get", "configmap", "ambit-oauth-rotation", "-o", "jsonpath={.data}"))
hydra_up()
time.sleep(8)  # ambit-auth reads the time every 3 seconds
st, j = token(cidA, grant_type="refresh_token", refresh_token=tokA["refresh_token"])
check("E2 after Hydra is back, and before the rotation is run again, the refresh of an older grant is already refused", st in (400, 401, 403) and "access_token" not in j, st)

# R1 · the rotation (the block is on: every token is refused while the keys change)
t_rot = time.time()
code, rot = ctl_json("rotate")
check("R1 the rotation answers 200 with the old and the new key of each signing set", code == "200" and len(rot.get("sets", [])) == 2 and all(s["oldKid"] != s["newKid"] for s in rot["sets"]), (code, rot.get("sets")))
acc = [s for s in rot.get("sets", []) if s["set"] == "hydra.jwt.access-token"]
newKid = acc[0]["newKid"] if acc else None
check("R1 the old key is the one that signed the token", acc and acc[0]["oldKid"] == kidA, (acc, kidA))

# R2 · the published keys are the new key only; Hydra's pods stay ready (one key per set)
new_kids = sorted(x["newKid"] for x in rot.get("sets", []))
check("R2 the published keys are the new keys only (one per set), and the old keys are not published", wait(lambda: jwks_kids() == new_kids, 60, 2), (jwks_kids(), new_kids))
def hydra_ready():
    r = kube("get", "pods", "-l", "app.kubernetes.io/name=ambit-hydra", "-o", "jsonpath={.items[*].status.containerStatuses[*].ready}").split()
    return len(r) >= 4 and all(x == "true" for x in r)
check("R2 Hydra's pods stay ready (each signing set holds exactly one key)", wait(hydra_ready, 120, 3))

# R3 · a new connection gets a token with the new key ID. (A grant made in the second of the rotation is over too: the time is in whole
# seconds. A person who connects within that second connects again; the test waits it out.)
time.sleep(3)
cidB, tokB = connect_wait("alice", ("time",), "AfterRotation")
check("R3 a new connection's token carries the new key ID", header(tokB["access_token"]).get("kid") == newKid, (header(tokB["access_token"]).get("kid"), newKid))

# R4 · the refresh of an old grant is refused
st, j = token(cidC, grant_type="refresh_token", refresh_token=tokC["refresh_token"])
check("R4 the refresh of a grant made before the rotation is refused (a grant that nothing else has ended)", st in (400, 401, 403) and "access_token" not in j, st)

# R5 · the check on every gateway pod (the block is on, so only the signature decides 401 or 403)
probe_old, probe_new = rot["probe"]["old"], rot["probe"]["new"]
id_old, id_new = rot["idTokenProbe"]["old"], rot["idTokenProbe"]["new"]  # the gateways trust the ID token key too
done, t = {}, time.time()
while time.time() - t < 600 and len(done) < len(PORTS):
    for p in PORTS:
        if p not in done and code_of(p, probe_old) == 401 and code_of(p, probe_new) == 403 and code_of(p, id_old) == 401 and code_of(p, id_new) == 403:
            done[p] = time.time() - t_rot
    time.sleep(2)
check("R5 on every pod: a token of an old key (of either signing set) is 401 and a token of a new key is 403 (valid, refused by the rules)", len(done) == len(PORTS), done)
check("E1 the block held all the time: a real token is refused on every pod", all_pods(lambda p: tools(p, tokB["access_token"])[0] in (401, 403)))
t_all = max(done.values()) if done else float("nan")
print(f"   emergency rotation measured: the old key is refused and the new key accepted on every pod {t_all:.0f} s after the rotation request", flush=True)

# R6 · the block is lifted: new tokens work, old ones do not
block_off()
check("R6 after the block is lifted the new connection works on every pod", wait(lambda: all_pods(lambda p: tools(p, tokB["access_token"])[0] == 200), 60, 1))
check("R6 the token of the old key is refused with 401 on every pod", all_pods(lambda p: code_of(p, tokA["access_token"]) == 401))
check("R6 API keys work on the same route", all_pods(lambda p: tools(p, key=KEY)[0] == 200))

# R7 · a grant made after the rotation refreshes, with the new key ID
st, j = token(cidB, grant_type="refresh_token", refresh_token=tokB["refresh_token"])
check("R7 a new grant refreshes and the token carries the new key ID", st == 200 and j.get("access_token") and header(j["access_token"]).get("kid") == newKid, st)
check("R7 the refreshed token works on every pod", accepted_everywhere(j["access_token"], 60))
code, st1 = ctl_json("keys")
check("E1 the status shows the new keys and the time of the rotation", code == "200" and "lastRotation" in st1 and sorted(st1.get("published", [])) == new_kids and kidA not in st1.get("published", []), (code, st1))

print()
print("FAILED: " + ", ".join(fails) if fails else "all checks passed")
sys.exit(1 if fails else 0)
