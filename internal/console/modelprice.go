package console

import (
	"net/http"
	"regexp"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftsman-apps/ambit/internal/pricing"
)

type PriceInput struct {
	Input  string `json:"inputPerMillionTokens"`
	Output string `json:"outputPerMillionTokens"`
	Remove bool   `json:"remove"`
}

func (c *Catalog) modelPrice(w http.ResponseWriter, r *http.Request, _ Session) {
	var m v1.Model
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("name")}, &m); err != nil {
		apiErr(w, err)
		return
	}
	out := map[string]any{"price": m.Spec.Price, "suggestion": nil, "listError": "", "licenceURL": "/api/prices/licence"}
	if c.Prices != nil {
		l, lerr := c.Prices.Current()
		out["listError"] = lerr
		if s, ok := l.Suggest(m); ok {
			out["suggestion"] = s
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *Catalog) setModelPrice(w http.ResponseWriter, r *http.Request, s Session) {
	var in PriceInput
	if !decodeInto(w, r, &in) {
		return
	}
	var price *v1.ModelPrice
	if !in.Remove {
		for _, p := range []string{in.Input, in.Output} {
			if _, err := pricing.PerTokenUnits(p); err != nil || !priceRE.MatchString(p) {
				writeErr(w, http.StatusBadRequest, "a price is dollars per million tokens, like 0.15 (up to 6 digits before and after the point)")
				return
			}
		}
		price = &v1.ModelPrice{InputPerMillionTokens: in.Input, OutputPerMillionTokens: in.Output}
	}
	var m v1.Model
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("name")}, &m); err != nil {
		apiErr(w, err)
		return
	}
	m.Spec.Price = price
	if c.GitOps != nil {
		c.propose(w, r, s, "update", "Model", "models", m.Name, m.Spec, m.Labels, consoleAnnotations(m.Annotations))
		return
	}
	// The controller writes the Model's status whenever budgets or rules change; a write between a read and an
	// update is a conflict. The price applies to the newest object too, so read it again and retry.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Client.Get(r.Context(), client.ObjectKeyFromObject(&m), &m); err != nil {
			return err
		}
		m.Spec.Price = price
		return c.Client.Update(r.Context(), &m)
	})
	if err != nil {
		if apierrors.IsConflict(err) {
			writeErr(w, http.StatusConflict, "the model kept changing while the price was saved; try again")
			return
		}
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"price": price})
}

// priceRE is the Model CRD's price pattern.
var priceRE = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,6})?$`)
