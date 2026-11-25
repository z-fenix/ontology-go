package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Tuple is an OpenFGA relationship tuple.
type Tuple struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

// TupleWriter writes and deletes relationship tuples.
type TupleWriter interface {
	WriteTuples(ctx context.Context, writes, deletes []Tuple) error
}

// maxTuplesPerWrite is OpenFGA's default per-request limit.
const maxTuplesPerWrite = 100

// WriteTuples calls POST /stores/{id}/write. Writing an existing tuple or deleting a missing one
// is tolerated (OpenFGA 1.8 "ignore" semantics), so the sync can be retried safely.
func (f *OpenFGA) WriteTuples(ctx context.Context, writes, deletes []Tuple) error {
	for len(writes) > 0 || len(deletes) > 0 {
		n := maxTuplesPerWrite
		var w, d []Tuple
		take := func(src *[]Tuple) []Tuple {
			k := len(*src)
			if k > n {
				k = n
			}
			out := (*src)[:k]
			*src = (*src)[k:]
			n -= k
			return out
		}
		d = take(&deletes)
		w = take(&writes)
		if err := f.write(ctx, w, d); err != nil {
			return err
		}
	}
	return nil
}

func (f *OpenFGA) write(ctx context.Context, writes, deletes []Tuple) error {
	body := map[string]any{}
	if f.ModelID != "" {
		body["authorization_model_id"] = f.ModelID
	}
	if len(writes) > 0 {
		body["writes"] = map[string]any{"tuple_keys": writes, "on_duplicate": "ignore"}
	}
	if len(deletes) > 0 {
		body["deletes"] = map[string]any{"tuple_keys": deletes, "on_missing": "ignore"}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/stores/%s/write", f.APIURL, f.StoreID), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("openfga write: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
