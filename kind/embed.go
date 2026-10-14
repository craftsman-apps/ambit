// Package kind embeds the kind cluster file of the quick start, so `ambit local up` and the release asset
// kind-cluster.yaml are the same file.
package kind

import _ "embed"

//go:embed cluster.yaml
var ClusterYAML []byte
