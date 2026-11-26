#!/usr/bin/env python3
"""End-to-end checks of the emergency block as an object of its own (test/stepoauth.sh check 18, after rotate.py). Standard library only.

The old runbook blocked tokens by editing the policy that Helm renders (ambit-mcp-public-oauth); Argo CD self-heal puts that policy back within seconds, so the
block disappeared without a word. The block is now the policy ambit-oauth-emergency-block, made by `ambit oauth block on`, which no chart and no Git repository
contains. Checked: N1a a re-apply of the chart (what Argo CD self-heal does) undoes the OLD kind of block (the control: the test's re-apply works);
N1b `ambit oauth block on` refuses every token on every pod at once and not a key; N1c a helm upgrade, N1d and a re-apply of the chart leave it;
N1e status names it; N1f `off` lifts it.

Environment: as gateway.py, plus BLOCKCTL (the ambit binary), NS, HELM_UPGRADE (a command that runs the Helm upgrade of the release) and HELM_TEMPLATE (a command
that prints the manifests of the chart with the release's values; its AgentgatewayPolicy objects are applied the way Argo CD applies them)."""
import os, re
src = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "gateway.py")).read().split("# 1 · no token")[0]
exec(src)

CTL = os.environ["BLOCKCTL"]
NSN = os.environ["NS"]
POL = "agentgatewaypolicy/ambit-mcp-public-oauth"
BLOCK = "agentgatewaypolicy/ambit-oauth-emergency-block"
tok = {}


def sh(cmd, input=None):
    r = subprocess.run(cmd, shell=True, input=input, capture_output=True, text=True, timeout=900)
    return r.returncode, r.stdout, r.stderr


def ambit(*a):
    r = subprocess.run([CTL, "-n", NSN, *a], capture_output=True, text=True, timeout=120)
    return r.returncode, (r.stdout + r.stderr)


def renew():
    """A new access token of the connection (the old one lives 15 minutes; the helm upgrade takes a while)."""
    st, j = token(tok["cid"], grant_type="refresh_token", refresh_token=tok["refresh_token"])
    if st == 200 and j.get("access_token"):
        tok.update(access_token=j["access_token"], refresh_token=j.get("refresh_token", tok["refresh_token"]))
    return st == 200


def code_all(fn):
    return [fn(p) for p in PORTS]


def works():
    return all(tools(p, tok["access_token"])[0] == 200 for p in PORTS)


def refused():
    return all(tools(p, tok["access_token"])[0] in (401, 403) for p in PORTS)


def blocked():
    """403 on every pod: the rules refuse a token that is valid (a token that has expired is 401, and would prove nothing)."""
    return all(tools(p, tok["access_token"])[0] == 403 for p in PORTS)


def reapply():
    """What Argo CD self-heal does: apply the manifests of the chart again (server-side, forcing the field manager); only its AgentgatewayPolicy objects here."""
    rc, manifest, err = sh(os.environ["HELM_TEMPLATE"])
    if rc != 0:
        return False, err[-300:]
    docs = [d for d in re.split(r"(?m)^---\s*$", manifest) if re.search(r"(?m)^kind: AgentgatewayPolicy\s*$", d)]
    rc, out, err = sh("kubectl -n %s apply --server-side --force-conflicts --field-manager=argocd-controller -f -" % NSN, input="\n---\n".join(docs))
    return rc == 0, (out + err)[-300:]


def connect_wait(*a):
    end = time.time() + 120
    while True:
        try: return connect(*a)
        except RuntimeError as e:
            if "503" not in str(e) or time.time() > end: raise
            time.sleep(3)


# ---------------------------------------------------------------------------------------------------------------------
cid, t0 = connect_wait("alice", ("time",), "BlockTest")
tok.update(cid=cid, access_token=t0["access_token"], refresh_token=t0["refresh_token"])
check("N0 a connection works on every pod before anything is blocked", accepted_everywhere(tok["access_token"], 120))

# N1a · the control: the OLD block (a clause in the policy that Helm renders) is undone by a re-apply of the chart, as Argo CD self-heal does it
kube("patch", POL, "--type", "json", "-p", '[{"op":"add","path":"/spec/traffic/authorization/policy/matchExpressions/-","value":"false"}]')
check("N1a the old kind of block (an edit of the chart's policy) refuses every token", wait(refused, 30, 1))
ok, msg = reapply()
check("N1a the re-apply of the chart's policies works", ok, msg)
check("N1a the re-apply undoes the old kind of block: the token works again (this is the defect that the new block does not have)", wait(works, 90, 2))

# N1b · the new block
t_on = time.time()
rc, text = ambit("oauth", "block", "on", "--yes")
check("N1b ambit oauth block on succeeds", rc == 0 and "blocked" in text, text[-300:])
seen, t = {}, time.time()
while time.time() - t < 60 and len(seen) < len(PORTS):
    for p in PORTS:
        if p not in seen and tools(p, tok["access_token"])[0] == 403:
            seen[p] = time.time() - t_on
    time.sleep(0.2)
check("N1b every token is refused with 403 on every pod", len(seen) == len(PORTS), seen)
print(f"   ambit oauth block on: refused on every pod {max(seen.values()) if seen else float('nan'):.1f} s after the command was started", flush=True)
check("N1b API keys keep working on the same route", all(tools(p, key=KEY)[0] == 200 for p in PORTS))
rc, text = sh("helm get manifest ambit -n %s" % NSN)[0:2]
check("N1b the policy is in no Helm release", "ambit-oauth-emergency-block" not in text)

# N1c · a Helm upgrade leaves it
rc, out, err = sh(os.environ["HELM_UPGRADE"])
check("N1c the Helm upgrade succeeds", rc == 0, err[-300:])
check("N1c after the Helm upgrade the block is still there (the policy exists)", kube("get", BLOCK, "-o", "name") != "")
check("N1c a new token of the connection (the token endpoint is not blocked)", wait(renew, 180, 3))
check("N1c ... is refused with 403 on every pod: the block holds after the Helm upgrade", blocked())

# N1d · a re-apply of the chart (Argo CD self-heal) leaves it
ok, msg = reapply()
check("N1d the re-apply of the chart works", ok, msg)
time.sleep(15)
check("N1d a new token of the connection", wait(renew, 60, 3))
check("N1d after the re-apply the policy exists and the token is still refused with 403 on every pod", kube("get", BLOCK, "-o", "name") != "" and blocked())

# N1e · status names it
rc, text = ambit("oauth", "block", "status")
check("N1e status says that tokens are blocked by the policy", rc == 0 and "BLOCKED by the policy ambit-oauth-emergency-block" in text, text[-300:])

# N1f · off lifts it
rc, text = ambit("oauth", "block", "off", "--yes")
check("N1f ambit oauth block off succeeds", rc == 0 and "lifted" in text, text[-300:])
check("N1f the policy is gone", kube("get", BLOCK, "-o", "name") == "")
renewed = renew()
check("N1f after the block is lifted a connection works again on every pod", renewed and wait(works, 60, 1))
rc, text = ambit("oauth", "block", "status")
check("N1f status says that tokens are not blocked", "not blocked" in text, text[-200:])

print()
print("FAILED: " + ", ".join(fails) if fails else "all checks passed")
sys.exit(1 if fails else 0)
