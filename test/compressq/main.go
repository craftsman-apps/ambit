// compressq is a test MCP server for the answer-quality check of result compression (issue #205). Its tools return
// fixed fixtures; `compressq -questions` prints the 20 questions as JSON for test/compressq/run.py. Test only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	printQ := flag.Bool("questions", false, "print the questions as JSON and exit")
	flag.Parse()
	if *printQ {
		if err := json.NewEncoder(os.Stdout).Encode(questions); err != nil {
			log.Fatal(err)
		}
		return
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "cq", Version: "1"}, nil)
	text := func(name, desc string, f func() string) {
		mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: f()}}}, nil, nil
			})
	}
	text("issues", "List the open and closed issues of a project as pretty-printed JSON.", issuesText)
	text("rows", "Return rows of a job table as pretty-printed JSON.", rowsText)
	text("log", "Return the service log of the last minutes.", logText)
	text("tests", "Run the unit tests of the package and return the output of go test -v.", testsText)
	text("csv", "Export customer orders as CSV.", csvText)
	text("table", "Return the service health report as a Markdown table.", tableText)
	text("pods", "Return the pods of the cluster as YAML.", podsText)
	// Both a text and a structured result, with different content: which one does a client send to the model?
	mcp.AddTool(s, &mcp.Tool{Name: "both", Description: "Return a short report."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: bothText()}}}, bothStructured(), nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	fmt.Fprintln(os.Stderr, "compressq listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
