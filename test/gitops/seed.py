# Writes the test catalogs as one file per object into <repo>/ambit/<plural>/<name>.yaml, the layout
# the console uses. usage: uv run --no-project --with pyyaml python test/gitops/seed.py <checkout dir>
import pathlib, sys, yaml
out = pathlib.Path(sys.argv[1]) / "ambit"
plural = {"Model": "models", "Server": "servers", "AccessRule": "accessrules"}
for src in ["test/llm/objects.yaml", "test/mcp/objects.yaml"]:
    for doc in yaml.safe_load_all(open(src)):
        if not doc or doc.get("apiVersion", "").split("/")[0] != "ambitproject.dev":
            continue
        doc["metadata"].pop("namespace", None)
        p = out / plural[doc["kind"]] / (doc["metadata"]["name"] + ".yaml")
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(yaml.safe_dump(doc, sort_keys=False))
