# Prints a GitHub App installation token (or, with --installation-id, the installation ID) for one
# repository. Used by the step 4c test to seed the config repository and to merge pull requests.
# usage: uv run --no-project --with 'pyjwt[crypto]' python test/gitops/github_app.py APP_ID KEY_FILE owner/repo [--installation-id]
import json, sys, time, urllib.request
import jwt

app_id, key_file, repo = sys.argv[1:4]
now = int(time.time())
app_jwt = jwt.encode({"iss": app_id, "iat": now - 60, "exp": now + 540}, open(key_file).read(), algorithm="RS256")

def call(method, path, body=None):
    req = urllib.request.Request("https://api.github.com" + path, method=method, data=json.dumps(body).encode() if body else None,
                                 headers={"Authorization": "Bearer " + app_jwt, "Accept": "application/vnd.github+json"})
    with urllib.request.urlopen(req) as r:
        return json.load(r)

inst = call("GET", f"/repos/{repo}/installation")["id"]
if "--installation-id" in sys.argv:
    print(inst)
else:
    print(call("POST", f"/app/installations/{inst}/access_tokens", {"repositories": [repo.split("/")[1]]})["token"])
