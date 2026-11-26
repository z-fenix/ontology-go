package action

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLimiterTokenBucket(t *testing.T) {
	clock := time.Unix(0, 0)
	l := &Limiter{Default: RateLimit{PerMinute: 60, Burst: 2}, now: func() time.Time { return clock }}
	alice := Actor{ID: "alice", Kind: ActorUser}
	if l.Allow(alice, "a", 1) != 0 || l.Allow(alice, "a", 1) != 0 {
		t.Fatal("the burst must be allowed")
	}
	if wait := l.Allow(alice, "a", 1); wait <= 0 || wait > time.Second {
		t.Fatalf("third request: wait = %v, want (0, 1s]", wait)
	}
	if l.Allow(Actor{ID: "bob"}, "a", 1) != 0 || l.Allow(alice, "b", 1) != 0 {
		t.Error("buckets are per actor and per action type")
	}
	clock = clock.Add(time.Second)
	if l.Allow(alice, "a", 1) != 0 {
		t.Error("one token refills per second at 60/min")
	}
}

func TestLimiterOverridesAndBatches(t *testing.T) {
	agent := Actor{ID: "agent-1", Kind: ActorAgent}
	l := &Limiter{Default: RateLimit{}, Agents: &RateLimit{PerMinute: 6, Burst: 1},
		PerAction: map[string]RateLimit{"bulk": {PerMinute: 60, Burst: 5}}, now: func() time.Time { return time.Unix(0, 0) }}
	if l.Allow(Actor{ID: "u"}, "x", 1000) != 0 {
		t.Error("an empty default is unlimited")
	}
	if l.Allow(agent, "x", 1) != 0 || l.Allow(agent, "x", 1) == 0 {
		t.Error("agents use the agent limit")
	}
	if l.Allow(agent, "bulk", 50) != 0 {
		t.Error("a batch above the burst runs once the bucket is full")
	}
	if l.Allow(agent, "bulk", 1) == 0 {
		t.Error("the bucket is empty after a large batch")
	}
	var nilLimiter *Limiter
	if nilLimiter.Allow(agent, "x", 1) != 0 {
		t.Error("a nil limiter allows everything")
	}
}

func TestParseRateLimits(t *testing.T) {
	if rl, err := ParseRateLimit("60/10"); err != nil || rl.PerMinute != 60 || rl.Burst != 10 {
		t.Errorf("60/10 = %+v, %v", rl, err)
	}
	for _, bad := range []string{"x", "-1", "60/0", "60/x"} {
		if _, err := ParseRateLimit(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	m, err := ParseActionRateLimits("assignWorkOrder=30/5, groundAircraft=2")
	if err != nil || m["assignWorkOrder"].Burst != 5 || m["groundAircraft"].PerMinute != 2 {
		t.Errorf("per-action = %v, %v", m, err)
	}
	if _, err := ParseActionRateLimits("noequals"); err == nil {
		t.Error("missing = must be rejected")
	}
}

type fakeSweepStore struct {
	runs    []StuckRun
	touched []string
	marks   []string
}

func (f *fakeSweepStore) StuckRuns(context.Context, time.Duration, int) ([]StuckRun, error) {
	return f.runs, nil
}

func (f *fakeSweepStore) TouchRun(_ context.Context, r StuckRun) error {
	f.touched = append(f.touched, r.ActionRID)
	return nil
}

func (f *fakeSweepStore) MarkRun(_ context.Context, rid, status, code, _ string) error {
	f.marks = append(f.marks, rid+"|"+status+"|"+code)
	return nil
}

type sweepDispatcher struct {
	outcomes map[string]*WritebackOutcome
	errs     map[string]error
	started  []string
}

func (d *sweepDispatcher) RunWriteback(_ context.Context, rid string, _ time.Duration) (*WritebackOutcome, error) {
	return d.outcomes[rid], d.errs[rid]
}

func (d *sweepDispatcher) StartSideEffects(_ context.Context, rid string) error {
	d.started = append(d.started, rid)
	return nil
}

func TestSweeper(t *testing.T) {
	store := &fakeSweepStore{runs: []StuckRun{
		{"r-running", SweepWriteback}, {"r-done", SweepWriteback}, {"r-failed", SweepWriteback},
		{"r-broken", SweepWriteback}, {"r-effects", SweepSideEffects},
	}}
	d := &sweepDispatcher{
		outcomes: map[string]*WritebackOutcome{
			"r-done":   {Status: StatusCommitted},
			"r-failed": {Status: StatusFailed, ErrorCode: "WRITEBACK_FAILED"},
		},
		errs: map[string]error{"r-broken": errors.New("temporal unavailable")},
	}
	st, err := (&Sweeper{Store: store, Dispatcher: d}).SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := SweepStats{Writebacks: 4, Resolved: 2, SideEffects: 1, Errors: 1}
	if st != want {
		t.Errorf("stats = %+v, want %+v", st, want)
	}
	if strings.Join(store.touched, ",") != "r-running,r-effects" {
		t.Errorf("touched = %v", store.touched)
	}
	if strings.Join(store.marks, ",") != "r-failed|FAILED|WRITEBACK_FAILED" {
		t.Errorf("marks = %v", store.marks)
	}
	if strings.Join(d.started, ",") != "r-effects" {
		t.Errorf("side effects restarted = %v", d.started)
	}
}

type fakeAudit struct{ last RunFilter }

func (f *fakeAudit) ListRuns(_ context.Context, rf RunFilter) ([]RunSummary, *RunCursor, error) {
	f.last = rf
	return []RunSummary{{ActionRID: "r1", ActorID: rf.ActorID}},
		&RunCursor{CreatedAt: time.Unix(100, 0).UTC(), ActionRID: "r1"}, nil
}

func (f *fakeAudit) ListEdits(context.Context, string) ([]EditRecord, error) {
	return []EditRecord{{EditID: 1, ObjectType: "WorkOrder", ObjectPK: "WO-1", Op: "MODIFY"}}, nil
}

type runCommitter struct {
	fakeCommitter
	run *RunRecord
}

func (c *runCommitter) GetRun(context.Context, string) (*RunRecord, error) { return c.run, nil }

func auditHandler() (*Handler, *fakeAudit) {
	fa := &fakeAudit{}
	return &Handler{
		Committer:   &runCommitter{run: &RunRecord{ActionRID: "r1", ActorID: "agent-1", OnBehalfOf: "bob"}},
		Ontology:    "mro",
		Identity:    HeaderIdentity,
		Audit:       fa,
		AuditGroups: []string{"auditors"},
	}, fa
}

func call(h *Handler, path, actor, groups string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Actor", actor)
	if groups != "" {
		r.Header.Set("X-Actor-Groups", groups)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, r)
	return rec
}

func TestListRunsScopedToCaller(t *testing.T) {
	h, fa := auditHandler()
	rec := call(h, "/api/v1/ontologies/mro/actionRuns?status=COMMITTED&objectType=WorkOrder&objectPk=WO-1", "alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if fa.last.ActorID != "alice" || fa.last.Status != "COMMITTED" || fa.last.Object == nil || fa.last.Object.PK != "WO-1" {
		t.Errorf("filter = %+v", fa.last)
	}
	var body struct {
		NextPageToken string `json:"nextPageToken"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.NextPageToken == "" {
		t.Fatal("missing nextPageToken")
	}
	if rec := call(h, "/api/v1/ontologies/mro/actionRuns?pageToken="+body.NextPageToken, "alice", ""); rec.Code != http.StatusOK ||
		fa.last.After == nil || fa.last.After.ActionRID != "r1" {
		t.Errorf("page token not decoded: %d %+v", rec.Code, fa.last.After)
	}

	if rec := call(h, "/api/v1/ontologies/mro/actionRuns?actor=bob", "alice", ""); rec.Code != http.StatusForbidden {
		t.Errorf("listing another actor's runs: status = %d", rec.Code)
	}
	if rec := call(h, "/api/v1/ontologies/mro/actionRuns?actor=bob", "carol", "auditors"); rec.Code != http.StatusOK || fa.last.ActorID != "bob" {
		t.Errorf("auditor: status = %d, filter = %+v", rec.Code, fa.last)
	}
	for _, q := range []string{"since=yesterday", "objectType=WorkOrder", "pageSize=1000", "pageToken=%21"} {
		if rec := call(h, "/api/v1/ontologies/mro/actionRuns?"+q, "alice", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
}

func TestRunVisibility(t *testing.T) {
	h, _ := auditHandler()
	// the agent that ran it, the user it acted for, an auditor, and someone else
	for _, tc := range []struct {
		actor, groups string
		want          int
	}{
		{"agent-1", "", http.StatusOK},
		{"bob", "", http.StatusOK},
		{"carol", "auditors", http.StatusOK},
		{"mallory", "", http.StatusNotFound},
	} {
		for _, path := range []string{"/api/v1/ontologies/mro/actionRuns/r1", "/api/v1/ontologies/mro/actionRuns/r1/edits"} {
			if rec := call(h, path, tc.actor, tc.groups); rec.Code != tc.want {
				t.Errorf("%s %s: status = %d, want %d", tc.actor, path, rec.Code, tc.want)
			}
		}
	}
}

func TestApplyRateLimited(t *testing.T) {
	e, _, _ := fixture(t)
	h := &Handler{Engine: e, Committer: &fakeCommitter{}, Ontology: "mro", Identity: HeaderIdentity,
		Limiter: &Limiter{Default: RateLimit{PerMinute: 1, Burst: 1}}}
	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/ontologies/mro/actions/assignLocal/validate",
			strings.NewReader(`{"parameters":{"workOrder":"WO-1","technician":"T-1","note":"n"}}`))
		r.Header.Set("X-Actor", "alice")
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, r)
		return rec
	}
	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	rec := do()
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), "RATE_LIMITED") {
		t.Errorf("second: %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
}
