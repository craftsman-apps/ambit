// Package deploy embeds the files that `ambit local up` uses, so the CLI and the release files come from one copy.
package deploy

import "embed"

// Files holds quickstart.yaml, crds/*.yaml (Gateway API, agentgateway, ToolHive) and local/test-model.yaml.
//
//go:embed quickstart.yaml crds/*.yaml local/test-model.yaml
var Files embed.FS
