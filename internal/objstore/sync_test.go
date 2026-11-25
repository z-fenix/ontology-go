package objstore

import (
	"reflect"
	"testing"
	"time"

	"github.com/acme/ontology/internal/spec"
)

func TestPlanMerge(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	edited := func(props ...string) map[string]bool {
		m := map[string]bool{}
		for _, p := range props {
			m[p] = true
		}
		return m
	}
	row := func(ts time.Time) SourceRow {
		return SourceRow{PK: "A", TS: ts, Props: map[string]any{"status": "ACTIVE", "model": "A320"}}
	}
	cases := []struct {
		name      string
		strategy  spec.ConflictStrategy
		row       SourceRow
		cur       *currentState
		wantWrite []string
		wantClear []string
		wantSkip  bool
	}{
		{"new object", spec.ApplyUserEdits, row(t0), nil, []string{"model", "status"}, nil, false},
		{"edits win", spec.ApplyUserEdits, row(t0.Add(time.Hour)),
			&currentState{edited: edited("status"), editTS: &t0}, []string{"model"}, nil, false},
		{"older source keeps edit", spec.MostRecentValue, row(t0.Add(-time.Hour)),
			&currentState{edited: edited("status"), editTS: &t0}, []string{"model"}, nil, false},
		{"newer source wins", spec.MostRecentValue, row(t0.Add(time.Hour)),
			&currentState{edited: edited("status"), editTS: &t0}, []string{"model", "status"}, []string{"status"}, false},
		{"user delete sticks", spec.MostRecentValue, row(t0.Add(time.Hour)),
			&currentState{edited: edited(UserDeletedMarker), editTS: &t0, deleted: true}, nil, nil, true},
		{"source delete is undone", spec.ApplyUserEdits, row(t0),
			&currentState{edited: edited(), deleted: true}, []string{"model", "status"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planMerge(c.strategy, c.row, c.cur)
			if p.skip != c.wantSkip {
				t.Fatalf("skip = %v", p.skip)
			}
			if c.wantSkip {
				return
			}
			if got := propNames(p.write); !reflect.DeepEqual(got, c.wantWrite) {
				t.Errorf("write = %v, want %v", got, c.wantWrite)
			}
			if !reflect.DeepEqual(p.clear, c.wantClear) {
				t.Errorf("clear = %v, want %v", p.clear, c.wantClear)
			}
		})
	}
}
