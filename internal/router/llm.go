package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/craftsman-apps/ambit/internal/routerconfig"
)

// LLM asks an OpenAI-compatible chat completions endpoint for one category name.
type LLM struct{ HTTP *http.Client }

// Prompt asks for exactly one category name. The user's text goes in its own message, never into this one.
func Prompt(r routerconfig.Router) string {
	var b strings.Builder
	b.WriteString("Classify the user's message into exactly one category. Answer with the category name only.\nCategories:\n")
	for _, c := range r.Categories {
		fmt.Fprintf(&b, "%s: %s\n", c.Name, c.Description)
	}
	b.WriteString("other: anything else\n")
	return b.String()
}

func (l *LLM) Classify(ctx context.Context, r routerconfig.Router, key, text string) (string, int, error) {
	reqBody, _ := json.Marshal(map[string]any{"model": r.LLM.Model, "temperature": 0, "max_tokens": 8,
		"messages": []any{map[string]any{"role": "system", "content": Prompt(r)}, map[string]any{"role": "user", "content": text}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.LLM.Endpoint+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		if !strings.HasPrefix(key, "Bearer ") {
			key = "Bearer " + key
		}
		req.Header.Set("Authorization", key)
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("classifier answered HTTP %d", resp.StatusCode) // never the body: it may echo the key
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", 0, errors.New("classifier answer is not JSON")
	}
	if len(out.Choices) == 0 {
		return "", out.Usage.TotalTokens, nil
	}
	return out.Choices[0].Message.Content, out.Usage.TotalTokens, nil
}
