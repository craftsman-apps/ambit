package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// ModelsURL is OpenRouter's public model list; it needs no key (spec 4.1.1).
const ModelsURL = "https://openrouter.ai/api/v1/models"

// Preferred free models, best first. Taken from ModelsURL on 2026-10-14 (spec 11, decision 3); update it when
// OpenRouter drops one. The first is the model docs/quick-start.md used.
var Preferred = []string{
	"nvidia/nemotron-3-super-120b-a12b:free",
	"google/gemma-4-31b-it:free",
	"nvidia/nemotron-3-ultra-550b-a55b:free",
	"google/gemma-4-26b-a4b-it:free",
}

// MaxCandidates is how many free models local up tries before it gives up (spec 4.1.1).
const MaxCandidates = 3

// FreeModel is one entry of OpenRouter's list whose id ends in ":free".
type FreeModel struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
}

// FetchFreeModels reads the model list and keeps the models whose id ends in ":free".
func FetchFreeModels(ctx context.Context, c *http.Client, url string) ([]FreeModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	var list struct{ Data []FreeModel }
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	var free []FreeModel
	for _, m := range list.Data {
		if strings.HasSuffix(m.ID, ":free") {
			free = append(free, m)
		}
	}
	return free, nil
}

// Candidates returns up to MaxCandidates model ids to try, in order: the preferred models that are present, then
// the other free models with the largest context window first. override (--model) is the only candidate when set.
func Candidates(free []FreeModel, override string) ([]string, error) {
	if override != "" {
		return []string{override}, nil
	}
	present := map[string]bool{}
	for _, m := range free {
		present[m.ID] = true
	}
	var out []string
	for _, id := range Preferred {
		if present[id] {
			out = append(out, id)
		}
	}
	rest := append([]FreeModel(nil), free...)
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].ContextLength > rest[j].ContextLength })
	for _, m := range rest {
		if !contains(out, m.ID) {
			out = append(out, m.ID)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("OpenRouter lists no free model")
	}
	if len(out) > MaxCandidates {
		out = out[:MaxCandidates]
	}
	return out, nil
}

// TryCandidates calls try for each candidate until one succeeds. It returns the chosen id, or an error that names
// every candidate and why it failed.
func TryCandidates(cands []string, try func(id string) error) (string, error) {
	var failures []string
	for _, id := range cands {
		err := try(id)
		if err == nil {
			return id, nil
		}
		failures = append(failures, id+": "+err.Error())
	}
	return "", fmt.Errorf("no free model answered: %s", strings.Join(failures, "; "))
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
