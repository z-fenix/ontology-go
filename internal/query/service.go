// Package query serves object reads (Spec §8.3): single objects, link traversal, filtered and
// sorted object sets, and aggregations — all with object-level permissions and property redaction.
//
// Permission model:
//   - Object types with an authz block are filtered per object through the "viewer" relation;
//     an object the caller may not view behaves exactly like a missing one.
//   - Properties above the caller's clearance are returned as null and listed in __redacted.
//   - A filter, sort or aggregation may not reference a property above the caller's clearance
//     (otherwise its value could be inferred from which objects match).
package query

import (
	"context"
	"errors"
	"net/http"

	"github.com/acme/ontology/internal/authz"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"
)

// Limits.
const (
	MaxPageSize = 1000
	// MaxScan bounds how many candidate rows a permission-filtered search or aggregation reads.
	MaxScan = 10000
	// MaxAggregateObjects bounds aggregations over authz-protected object types, which must
	// list visible objects first.
	MaxAggregateObjects = 10000
)

// Caller is the authenticated principal making a request.
type Caller struct {
	ID         string
	Kind       string // USER | AGENT | SERVICE
	OnBehalfOf string
	Groups     []string
}

// OntologySource resolves compiled ontologies (satisfied by *omsclient.Cache).
type OntologySource interface {
	Active(ctx context.Context) (*spec.CompiledOntology, error)
	Get(ctx context.Context, version int64) (*spec.CompiledOntology, error)
}

// Store is the object storage used by the service (satisfied by *objstore.Store).
type Store interface {
	GetMany(ctx context.Context, ont *spec.CompiledOntology, refs []objstore.ObjectRef) (map[objstore.ObjectRef]*objstore.Object, error)
	Links(ctx context.Context, ont *spec.CompiledOntology, from objstore.ObjectRef, nav string, limit int) ([]*objstore.Object, error)
	Search(ctx context.Context, ont *spec.CompiledOntology, typ string, q objstore.SearchQuery) ([]*objstore.Object, bool, error)
	Aggregate(ctx context.Context, ont *spec.CompiledOntology, typ string, q objstore.AggQuery) ([]objstore.AggRow, bool, error)
}

// Service answers object queries.
type Service struct {
	Ontologies OntologySource
	Store      Store
	Authz      authz.Authorizer
	Clearances authz.Clearances
}

// ObjectView is an object as returned to a caller.
type ObjectView struct {
	Type       string         `json:"type"`
	PK         string         `json:"pk"`
	RID        string         `json:"rid"`
	Version    int64          `json:"version"`
	Properties map[string]any `json:"properties"`
	Redacted   []string       `json:"__redacted,omitempty"`
}

// Page is one page of objects.
type Page struct {
	Data            []ObjectView `json:"data"`
	NextPageToken   string       `json:"nextPageToken,omitempty"`
	OntologyVersion int64        `json:"ontologyVersion"`
}

func notFound(ref objstore.ObjectRef) error {
	return apperr.New(http.StatusNotFound, "OBJECT_NOT_FOUND", "object %s not found", ref)
}

func (s *Service) ontology(ctx context.Context, version *int64) (*spec.CompiledOntology, error) {
	if version != nil {
		o, err := s.Ontologies.Get(ctx, *version)
		if err != nil {
			return nil, apperr.New(http.StatusNotFound, "ONTOLOGY_NOT_FOUND", "ontology version %d not found", *version).Wrap(err)
		}
		return o, nil
	}
	o, err := s.Ontologies.Active(ctx)
	if err != nil {
		return nil, apperr.New(http.StatusServiceUnavailable, "ONTOLOGY_UNAVAILABLE", "no active ontology").Wrap(err)
	}
	return o, nil
}

func objectType(ont *spec.CompiledOntology, typ string) (*spec.ObjectType, error) {
	ot := ont.ObjectTypes[typ]
	if ot == nil {
		return nil, apperr.New(http.StatusNotFound, "OBJECT_TYPE_NOT_FOUND", "object type %q not found", typ)
	}
	return ot, nil
}

// visible reports whether the caller may view an object. Agents need both themselves and the
// user they act for to be allowed (Spec §8.2). Authorization outages fail closed.
func (s *Service) visible(ctx context.Context, c Caller, ot *spec.ObjectType, pk string) (bool, error) {
	if ot.Authz == nil {
		return true, nil
	}
	object := ot.Authz.FGAType + ":" + pk
	check := func(user string) (bool, error) {
		ok, err := s.Authz.Check(ctx, "user:"+user, "viewer", object)
		if err != nil {
			return false, apperr.New(http.StatusServiceUnavailable, "AUTHZ_UNAVAILABLE", "authorization check failed").Wrap(err)
		}
		return ok, nil
	}
	ok, err := check(c.ID)
	if err != nil || !ok || c.Kind != "AGENT" {
		return ok, err
	}
	if c.OnBehalfOf == "" {
		return false, nil
	}
	return check(c.OnBehalfOf)
}

func (s *Service) view(c Caller, ot *spec.ObjectType, o *objstore.Object) ObjectView {
	props, red := authz.Redact(ot, o.Props, s.Clearances.Level(c.Groups))
	return ObjectView{Type: o.Ref.Type, PK: o.Ref.PK, RID: o.RID, Version: o.Version, Properties: props, Redacted: red}
}

// checkFields rejects queries that read properties above the caller's clearance.
func (s *Service) checkFields(c Caller, ont *spec.CompiledOntology, fields []objstore.QueryField) error {
	level := s.Clearances.Level(c.Groups)
	for _, f := range fields {
		ot := ont.ObjectTypes[f.Type]
		if ot == nil {
			continue
		}
		p := ot.Properties[f.Field]
		if p == nil {
			continue
		}
		if _, red := authz.Redact(ot, map[string]any{f.Field: true}, level); len(red) > 0 {
			return apperr.New(http.StatusForbidden, "FIELD_NOT_PERMITTED",
				"filtering, sorting or aggregating on %s.%s (%s) requires a higher clearance", f.Type, f.Field, p.Sensitivity)
		}
	}
	return nil
}

func queryErr(err error) error {
	var ae *apperr.Error
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, objstore.ErrInvalidQuery), errors.Is(err, objstore.ErrInvalidValue):
		return apperr.BadRequest("INVALID_QUERY", "%v", err)
	case errors.Is(err, objstore.ErrUnknownType):
		return apperr.New(http.StatusNotFound, "NOT_FOUND", "%v", err)
	case errors.Is(err, objstore.ErrNotFound):
		return apperr.New(http.StatusNotFound, "OBJECT_NOT_FOUND", "%v", err)
	}
	return err
}

// Get returns one object.
func (s *Service) Get(ctx context.Context, c Caller, version *int64, typ, pk string) (*ObjectView, int64, error) {
	ont, err := s.ontology(ctx, version)
	if err != nil {
		return nil, 0, err
	}
	ot, err := objectType(ont, typ)
	if err != nil {
		return nil, 0, err
	}
	ref := objstore.ObjectRef{Type: typ, PK: pk}
	found, err := s.Store.GetMany(ctx, ont, []objstore.ObjectRef{ref})
	if err != nil {
		return nil, 0, queryErr(err)
	}
	o := found[ref]
	if o == nil {
		return nil, 0, notFound(ref)
	}
	ok, err := s.visible(ctx, c, ot, pk)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, notFound(ref)
	}
	v := s.view(c, ot, o)
	return &v, ont.Version, nil
}

// Links returns the objects linked to (typ, pk) through navigation name nav.
func (s *Service) Links(ctx context.Context, c Caller, version *int64, typ, pk, nav string, limit int) (*Page, error) {
	if _, _, err := s.Get(ctx, c, version, typ, pk); err != nil {
		return nil, err // the source object must itself be visible
	}
	ont, err := s.ontology(ctx, version)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxPageSize {
		limit = MaxPageSize
	}
	objs, err := s.Store.Links(ctx, ont, objstore.ObjectRef{Type: typ, PK: pk}, nav, limit)
	if err != nil {
		return nil, queryErr(err)
	}
	page := &Page{Data: []ObjectView{}, OntologyVersion: ont.Version}
	for _, o := range objs {
		ot := ont.ObjectTypes[o.Ref.Type]
		ok, err := s.visible(ctx, c, ot, o.Ref.PK)
		if err != nil {
			return nil, err
		}
		if ok {
			page.Data = append(page.Data, s.view(c, ot, o))
		}
	}
	return page, nil
}

// SearchRequest is an object-set search.
type SearchRequest struct {
	Where           *objstore.Where    `json:"where,omitempty"`
	OrderBy         []objstore.OrderBy `json:"orderBy,omitempty"`
	PageSize        int                `json:"pageSize,omitempty"`
	PageToken       string             `json:"pageToken,omitempty"`
	OntologyVersion *int64             `json:"ontologyVersion,omitempty"`
}

// Search returns one page of visible objects matching the request.
func (s *Service) Search(ctx context.Context, c Caller, typ string, req SearchRequest) (*Page, error) {
	ont, err := s.ontology(ctx, req.OntologyVersion)
	if err != nil {
		return nil, err
	}
	ot, err := objectType(ont, typ)
	if err != nil {
		return nil, err
	}
	sortFields := make([]string, 0, len(req.OrderBy))
	for _, o := range req.OrderBy {
		sortFields = append(sortFields, o.Field)
	}
	fields, err := objstore.QueryFields(ont, typ, req.Where, sortFields)
	if err != nil {
		return nil, queryErr(err)
	}
	if err := s.checkFields(c, ont, fields); err != nil {
		return nil, err
	}
	size := req.PageSize
	if size <= 0 {
		size = 100
	}
	if size > MaxPageSize {
		size = MaxPageSize
	}
	offset, err := decodeToken(req.PageToken, ont.Version, queryHash(typ, req))
	if err != nil {
		return nil, err
	}
	page := &Page{Data: []ObjectView{}, OntologyVersion: ont.Version}
	// Read candidates in chunks; keep the visible ones. The page token is the offset of the next
	// unread candidate, so filtered-out objects never shift later pages.
	scanned := 0
	for len(page.Data) < size {
		chunk := size - len(page.Data)
		if ot.Authz != nil && chunk < 100 {
			chunk = 100 // over-fetch: some candidates may be invisible
		}
		objs, more, err := s.Store.Search(ctx, ont, typ, objstore.SearchQuery{Where: req.Where, OrderBy: req.OrderBy,
			Offset: offset, Limit: chunk})
		if err != nil {
			return nil, queryErr(err)
		}
		consumed := 0
		for _, o := range objs {
			consumed++
			ok, err := s.visible(ctx, c, ot, o.Ref.PK)
			if err != nil {
				return nil, err
			}
			if ok {
				page.Data = append(page.Data, s.view(c, ot, o))
				if len(page.Data) == size {
					break
				}
			}
		}
		offset += consumed
		scanned += consumed
		if consumed < len(objs) || more {
			page.NextPageToken = encodeToken(offset, ont.Version, queryHash(typ, req))
		} else {
			page.NextPageToken = ""
		}
		if !more || consumed < len(objs) || len(page.Data) == size {
			break
		}
		if scanned >= MaxScan {
			break // return what we have; the token continues the scan
		}
	}
	return page, nil
}

// AggregateRequest aggregates visible objects.
type AggregateRequest struct {
	Where           *objstore.Where   `json:"where,omitempty"`
	GroupBy         []string          `json:"groupBy,omitempty"`
	Metrics         []objstore.Metric `json:"metrics,omitempty"`
	Limit           int               `json:"limit,omitempty"`
	OntologyVersion *int64            `json:"ontologyVersion,omitempty"`
}

// AggregateResult is the aggregation output.
type AggregateResult struct {
	Groups          []objstore.AggRow `json:"groups"`
	Truncated       bool              `json:"truncated"`
	OntologyVersion int64             `json:"ontologyVersion"`
}

// Aggregate computes metrics over the objects the caller may see.
func (s *Service) Aggregate(ctx context.Context, c Caller, typ string, req AggregateRequest) (*AggregateResult, error) {
	ont, err := s.ontology(ctx, req.OntologyVersion)
	if err != nil {
		return nil, err
	}
	ot, err := objectType(ont, typ)
	if err != nil {
		return nil, err
	}
	extra := append([]string(nil), req.GroupBy...)
	for _, m := range req.Metrics {
		if m.Field != "" {
			extra = append(extra, m.Field)
		}
	}
	fields, err := objstore.QueryFields(ont, typ, req.Where, extra)
	if err != nil {
		return nil, queryErr(err)
	}
	if err := s.checkFields(c, ont, fields); err != nil {
		return nil, err
	}
	q := objstore.AggQuery{Where: req.Where, GroupBy: req.GroupBy, Metrics: req.Metrics, Limit: req.Limit}
	if ot.Authz != nil {
		// Aggregate only visible objects: list matching primary keys, filter them, then restrict.
		visible := []string{}
		offset := 0
		for {
			objs, more, err := s.Store.Search(ctx, ont, typ, objstore.SearchQuery{Where: req.Where, Offset: offset, Limit: MaxPageSize})
			if err != nil {
				return nil, queryErr(err)
			}
			for _, o := range objs {
				ok, err := s.visible(ctx, c, ot, o.Ref.PK)
				if err != nil {
					return nil, err
				}
				if ok {
					visible = append(visible, o.Ref.PK)
				}
			}
			offset += len(objs)
			if !more {
				break
			}
			if offset >= MaxAggregateObjects {
				return nil, apperr.New(http.StatusUnprocessableEntity, "AGGREGATION_TOO_LARGE",
					"aggregations over %s are limited to %d matching objects; narrow the filter", typ, MaxAggregateObjects)
			}
		}
		q.RestrictPKs = visible
	}
	rows, more, err := s.Store.Aggregate(ctx, ont, typ, q)
	if err != nil {
		return nil, queryErr(err)
	}
	if rows == nil {
		rows = []objstore.AggRow{}
	}
	return &AggregateResult{Groups: rows, Truncated: more, OntologyVersion: ont.Version}, nil
}
