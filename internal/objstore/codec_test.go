package objstore

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/acme/ontology/internal/spec"
)

func prop(base spec.BaseType) *spec.Property {
	return &spec.Property{APIName: "p", Type: spec.TypeRef{Base: base}}
}

func TestEncodeValue(t *testing.T) {
	ts := time.Date(2026, 11, 24, 6, 0, 0, 0, time.FixedZone("SGT", 8*3600))
	ok := []struct {
		base spec.BaseType
		in   any
		want any
	}{
		{spec.TString, "x", "x"},
		{spec.TInteger, 3, int64(3)},
		{spec.TInteger, float64(4), int64(4)},
		{spec.TInteger, json.Number("5"), int64(5)},
		{spec.TLong, int64(1 << 40), int64(1 << 40)},
		{spec.TDouble, 2, float64(2)},
		{spec.TBoolean, true, true},
		{spec.TDecimal, "12.50", "12.50"},
		{spec.TDecimal, 1.5, "1.5"},
		{spec.TDecimal, 7, "7"},
		{spec.TDate, "2026-11-24", "2026-11-24"},
		{spec.TDate, ts, "2026-11-23"},
		{spec.TTimestamp, ts, ts.UTC()},
		{spec.TTimestamp, "2026-11-24T06:00:00+08:00", ts.UTC()},
		{spec.TArray, []string{"A320", "B737"}, `["A320","B737"]`},
		{spec.TString, nil, nil},
	}
	for _, c := range ok {
		got, err := encodeValue(prop(c.base), c.in)
		if err != nil {
			t.Errorf("%s %v: unexpected error %v", c.base, c.in, err)
			continue
		}
		if gt, isT := got.(time.Time); isT {
			if !gt.Equal(c.want.(time.Time)) {
				t.Errorf("%s %v = %v, want %v", c.base, c.in, got, c.want)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s %v = %#v, want %#v", c.base, c.in, got, c.want)
		}
	}
	bad := []struct {
		base spec.BaseType
		in   any
	}{
		{spec.TString, 1},
		{spec.TInteger, 1.5},
		{spec.TInteger, int64(1 << 40)},
		{spec.TBoolean, "true"},
		{spec.TDecimal, "abc"},
		{spec.TDate, "24/11/2026"},
		{spec.TTimestamp, "yesterday"},
		{spec.TVector, []float64{1}},
	}
	for _, c := range bad {
		if _, err := encodeValue(prop(c.base), c.in); !errors.Is(err, ErrInvalidValue) {
			t.Errorf("%s %v: expected ErrInvalidValue, got %v", c.base, c.in, err)
		}
	}
}

func TestDecodeValue(t *testing.T) {
	if got := decodeValue(prop(spec.TInteger), int32(3)); got != int64(3) {
		t.Errorf("int32 -> %#v", got)
	}
	got := decodeValue(prop(spec.TArray), `["a","b"]`)
	arr, ok := got.([]any)
	if !ok || len(arr) != 2 || arr[0] != "a" {
		t.Errorf("json array -> %#v", got)
	}
}

func TestPK(t *testing.T) {
	if v, err := pkToValue(prop(spec.TLong), "42"); err != nil || v != int64(42) {
		t.Errorf("pkToValue = %v, %v", v, err)
	}
	if _, err := pkToValue(prop(spec.TLong), "x"); err == nil {
		t.Error("expected error")
	}
	if s := pkString(int64(42)); s != "42" {
		t.Errorf("pkString = %q", s)
	}
}
