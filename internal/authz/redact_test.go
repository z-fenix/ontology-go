package authz

import (
	"reflect"
	"testing"

	"github.com/acme/ontology/internal/spec"
)

func TestRedact(t *testing.T) { // E16 (library level)
	ot := &spec.ObjectType{Properties: map[string]*spec.Property{
		"title":         {Sensitivity: spec.SensPublic},
		"summary":       {Sensitivity: spec.SensInternal},
		"estimatedCost": {Sensitivity: spec.SensConfidential},
		"pin":           {Sensitivity: spec.SensRestricted},
	}}
	props := map[string]any{"title": "t", "summary": "s", "estimatedCost": "12.50", "pin": "1234"}

	out, red := Redact(ot, props, spec.SensInternal)
	if !reflect.DeepEqual(red, []string{"estimatedCost", "pin"}) {
		t.Errorf("redacted = %v", red)
	}
	if out["estimatedCost"] != nil || out["pin"] != nil || out["summary"] != "s" {
		t.Errorf("out = %v", out)
	}
	if props["estimatedCost"] != "12.50" {
		t.Error("input must not be modified")
	}
	if _, red := Redact(ot, props, spec.SensRestricted); len(red) != 0 {
		t.Errorf("RESTRICTED clearance redacted %v", red)
	}
}

func TestClearances(t *testing.T) {
	c, err := ParseClearances("grp-finance=CONFIDENTIAL, grp-sec=restricted")
	if err != nil {
		t.Fatal(err)
	}
	if c.Level(nil) != spec.SensInternal || c.Level([]string{"grp-finance"}) != spec.SensConfidential ||
		c.Level([]string{"grp-finance", "grp-sec"}) != spec.SensRestricted {
		t.Errorf("levels wrong: %v", c)
	}
	for _, bad := range []string{"g=SECRET", "nogroup", "=PUBLIC"} {
		if _, err := ParseClearances(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	if c, err := ParseClearances(""); err != nil || len(c) != 0 {
		t.Errorf("empty: %v %v", c, err)
	}
}
