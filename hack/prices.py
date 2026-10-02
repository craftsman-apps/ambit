"""Write internal/console/prices/litellm-prices.json from LiteLLM's price list at one commit.
Usage: uv run --no-project python hack/prices.py <litellm commit sha>
Keeps only chat-capable entries with input and output prices; records the commit and date."""
import json, sys, urllib.request, datetime
sha = sys.argv[1]
base = f"https://raw.githubusercontent.com/BerriAI/litellm/{sha}/"
data = json.load(urllib.request.urlopen(base + "model_prices_and_context_window.json", timeout=60))
out = {}
for key, e in data.items():
    if not isinstance(e, dict) or key == "sample_spec":
        continue
    i, o = e.get("input_cost_per_token"), e.get("output_cost_per_token")
    if isinstance(i, (int, float)) and isinstance(o, (int, float)) and i >= 0 and o >= 0 and e.get("litellm_provider"):
        out[key] = {"litellm_provider": e["litellm_provider"], "input_cost_per_token": i, "output_cost_per_token": o}
meta = {"commit": sha, "date": datetime.date.today().isoformat()}
json.dump({"_ambit": meta, **out}, open("internal/console/prices/litellm-prices.json", "w"), indent=0, sort_keys=True)
open("internal/console/prices/LICENSE", "w").write(urllib.request.urlopen(base + "LICENSE", timeout=60).read().decode())
print(f"{len(out)} models from {sha}")
