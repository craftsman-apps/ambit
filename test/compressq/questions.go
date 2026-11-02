package main

// A question has a class that decides how it is graded across modes (docs/concepts.md, "Measured savings"):
//
//	failure, error, count: shorten must answer as well as off (the release gate); data: every mode keeps the data;
//	omitted: shorten removes the detail, so "not shown" is as good as the right answer; a wrong guess is a loss.
type question struct {
	ID       string   `json:"id"`
	Class    string   `json:"class"`
	Tool     string   `json:"tool"`
	Prompt   string   `json:"prompt"`
	Expect   []string `json:"expect"`             // each must appear in the answer (lower case; commas removed)
	NotShown string   `json:"notShown,omitempty"` // regular expression of an honest "I cannot tell" answer (omitted)
}

func q(id, class, tool, prompt string, expect ...string) question {
	return question{ID: id, Class: class, Tool: tool, Prompt: "Use only the MCP tool cq_" + tool + " (no arguments, no other tool) and answer from its result only, in one short line. " + prompt, Expect: expect}
}

var questions = []question{
	q("q01", "data", "issues", "How many issues have the label bug? Answer with the number only.", "6"),
	q("q02", "data", "issues", "What is the title of the issue with number 41?", "add linter in module 41"),
	q("q03", "data", "issues", "How many issues have state closed? Answer with the number only.", "5"),
	q("q04", "data", "rows", "What is the amount of the row with id 117?", "829.87"),
	q("q05", "data", "rows", "How many rows have status failed? Answer with the number only.", "6"),
	q("q06", "error", "log", "How many ERROR lines does the log have? Answer with the number only.", "8"),
	q("q07", "error", "log", "What is the id=... value of the last ERROR line?", "9119"),
	q("q08", "error", "log", "What disk usage percent does the WARN line about disk report?", "91"),
	q("q09", "data", "log", "What is the timestamp of the last line of the log?", "10:13:19"),
	q("q10", "error", "log", "In which file and line is the stack trace of the third ERROR (counting from the top)?", "cache.go", "94"),
	{ID: "q11", Class: "failure", Tool: "tests", Prompt: "Use only the MCP tool cq_tests (no arguments, no other tool) and answer from its result only, in one short line. Which tests failed? List their names.", Expect: []string{"testparseheader", "testretrybackoff", "testcacheevict"}},
	q("q12", "failure", "tests", "What was the failure message of TestRetryBackoff (got and want)?", "3s", "4s"),
	q("q13", "count", "tests", "How many tests passed? Answer with the number only.", "220"),
	q("q14", "failure", "tests", "How long did the package example.com/app/pkg take, as printed in the summary line?", "0.412"),
	q("q15", "data", "csv", "What is the amount of the row with id 77?", "1.77"),
	q("q16", "data", "csv", "How many rows have category refund? Answer with the number only.", "22"),
	q("q17", "data", "table", "Which services have status degraded? List their names.", "service-37", "service-121", "service-166"),
	q("q18", "data", "pods", "How many pods are in the namespace staging? Answer with the number only.", "10"),
	q("q19", "data", "pods", "Which pod has restartCount 7?", "web-23"),
	{ID: "q20", Class: "omitted", Tool: "log", Prompt: "Use only the MCP tool cq_log (no arguments, no other tool) and answer from its result only, in one short line. In the INFO 'served request' line logged at 2026-03-01T10:10:03Z, what is the item number at the end of the path? If the log does not show that line, say 'not shown'.",
		Expect: []string{"21"}, NotShown: `not shown|not (visible|available|included|present)|omitted|cannot|can't|unable|no such|does not (show|contain|include)|doesn't (show|contain|include)`},
}
