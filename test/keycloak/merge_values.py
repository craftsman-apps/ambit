# Appends the connectors and env vars from the Connect SSO page's values to an existing values file,
# as an admin would by hand. usage: merge_values.py BASE.yaml SNIPPET.yaml OUT.yaml
import sys, yaml
base, snippet = yaml.safe_load(open(sys.argv[1])), yaml.safe_load(open(sys.argv[2]))
base.setdefault("sso", {}).setdefault("connectors", []).extend(snippet["sso"]["connectors"])
base.setdefault("dex", {}).setdefault("envVars", []).extend(snippet["dex"]["envVars"])
yaml.safe_dump(base, open(sys.argv[3], "w"), sort_keys=False)
