package omsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/acme/ontology/internal/spec"
)

// HTTPLoader loads compiled ontologies from a running OMS over HTTP.
type HTTPLoader struct {
	BaseURL  string // e.g. http://oms:8080
	Ontology string
	Client   *http.Client
}

// NewHTTPLoader builds an HTTPLoader with a default client.
func NewHTTPLoader(baseURL, ontology string) *HTTPLoader {
	return &HTTPLoader{BaseURL: baseURL, Ontology: ontology, Client: http.DefaultClient}
}

func (l *HTTPLoader) fetch(ctx context.Context, query string) (*spec.CompiledOntology, error) {
	url := fmt.Sprintf("%s/api/v1/ontologies/%s/compiled%s", l.BaseURL, l.Ontology, query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("oms %s: %s: %s", url, resp.Status, body)
	}
	var c spec.CompiledOntology
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadActive fetches the active compiled ontology.
func (l *HTTPLoader) LoadActive(ctx context.Context) (*spec.CompiledOntology, error) {
	return l.fetch(ctx, "")
}

// LoadVersion fetches a specific version.
func (l *HTTPLoader) LoadVersion(ctx context.Context, version int64) (*spec.CompiledOntology, error) {
	return l.fetch(ctx, fmt.Sprintf("?version=%d", version))
}
