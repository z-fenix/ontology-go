package query

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"github.com/acme/ontology/internal/platform/apperr"
)

// pageToken carries the scan position. It is bound to the query and ontology version so it
// cannot be replayed against a different filter or schema.
type pageToken struct {
	Offset  int    `json:"o"`
	Version int64  `json:"v"`
	Query   string `json:"q"`
}

func queryHash(typ string, req SearchRequest) string {
	b, _ := json.Marshal(struct {
		T string
		W any
		O any
	}{typ, req.Where, req.OrderBy})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func encodeToken(offset int, version int64, hash string) string {
	b, _ := json.Marshal(pageToken{Offset: offset, Version: version, Query: hash})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeToken(tok string, version int64, hash string) (int, error) {
	if tok == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	var pt pageToken
	if err != nil || json.Unmarshal(b, &pt) != nil || pt.Offset < 0 {
		return 0, apperr.BadRequest("INVALID_PAGE_TOKEN", "the page token is malformed")
	}
	if pt.Version != version || pt.Query != hash {
		return 0, apperr.BadRequest("INVALID_PAGE_TOKEN", "the page token belongs to a different query or ontology version; start over")
	}
	return pt.Offset, nil
}
