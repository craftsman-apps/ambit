// Package checks is the check catalogue of `ambit check` (spec 4.3): one function per check ID, each returning a
// Result, so the spec's table maps one-to-one to code and tests.
package checks

// Status is the result of one check.
type Status string

const (
	Pass    Status = "pass"
	Warn    Status = "warn"
	Fail    Status = "fail"
	Unknown Status = "unknown" // the CLI could not look: missing permission or an unreachable endpoint
	NA      Status = "n/a"     // the check does not apply
)

// Groups.
const (
	GroupPre        = "pre"
	GroupLocal      = "local"
	GroupHealth     = "health"
	GroupProduction = "production"
)

// Result is one line of `ambit check`, and one element of its JSON output.
type Result struct {
	ID      string `json:"id"`
	Group   string `json:"group"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Next    string `json:"next,omitempty"`
	Source  string `json:"source,omitempty"` // "values", "live objects", "values and live objects", "docker", "host"
}
