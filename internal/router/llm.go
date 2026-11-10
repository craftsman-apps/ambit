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

// Usage is what one classifier call used. A provider that reports only total_tokens has it all in In: the classifier
// writes at most 8 tokens, so pricing them as input is close enough for the Savings estimate.
type Usage struct{ In, Out int }

func (u Usage) Total() int { return u.In + u.Out }

func (l *LLM) Classify(ctx context.Context, r routerconfig.Router, key, text string) (string, Usage, error) {
	reqBody, _ := json.Marshal(map[string]any{"model": r.LLM.Model, "temperature": 0, "max_tokens": 8,
		"messages": []any{map[string]any{"role": "system", "content": Prompt(r)}, map[string]any{"role": "user", "content": text}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.LLM.Endpoint+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", Usage{}, err
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
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("classifier answered HTTP %d", resp.StatusCode) // never the body: it may echo the key
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", Usage{}, errors.New("classifier answer is not JSON")
	}
	u := Usage{In: max(out.Usage.PromptTokens, 0), Out: max(out.Usage.CompletionTokens, 0)}
	if u.Total() == 0 {
		u.In = max(out.Usage.TotalTokens, 0)
	}
	if len(out.Choices) == 0 {
		return "", u, nil
	}
	return out.Choices[0].Message.Content, u, nil
}
