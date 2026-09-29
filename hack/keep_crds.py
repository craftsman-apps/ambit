"""Add helm.sh/resource-policy: keep to every CRD in the given multi-document YAML files (in place).

Helm deletes CRDs it installed on `helm uninstall`, and Kubernetes then deletes every object of those
types. For Direct-mode customers that would erase all Models and AccessRules, so every CRD Ambit ships is kept.
Run with: uv run --no-project --with pyyaml python hack/keep_crds.py FILE...
"""
import sys
import yaml

for path in sys.argv[1:]:
    docs = [d for d in yaml.safe_load_all(open(path)) if d]
    for d in docs:
        if d.get("kind") == "CustomResourceDefinition":
            d.setdefault("metadata", {}).setdefault("annotations", {})["helm.sh/resource-policy"] = "keep"
    with open(path, "w") as f:
        f.write("# Generated; see hack/keep_crds.py. Do not edit by hand.\n")
        yaml.safe_dump_all(docs, f, sort_keys=False, width=1000)
    print(f"{path}: {sum(d.get('kind') == 'CustomResourceDefinition' for d in docs)} CRDs kept on uninstall")
