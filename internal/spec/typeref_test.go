package spec

import "testing"

func TestParseTypeRef(t *testing.T) {
    ok := map[string]string{
        "string":        "string",
        " long ":        "long",
        "vector(1024)":  "vector(1024)",
        "array<string>": "array<string>",
        "decimal(10,2)": "decimal(10,2)",
        "decimal":       "decimal(38,9)",
    }
    for in, want := range ok {
        tr, err := ParseTypeRef(in)
        if err != nil {
            t.Errorf("ParseTypeRef(%q) unexpected error: %v", in, err)
            continue
        }
        if got := tr.String(); got != want {
            t.Errorf("ParseTypeRef(%q).String() = %q, want %q", in, got, want)
        }
    }
    bad := []string{"", "vector", "vector(0)", "vector(x)", "array<array<string>>", "array", "decimal(2,5)", "decimal(1)", "strng"}
    for _, in := range bad {
        if _, err := ParseTypeRef(in); err == nil {
            t.Errorf("ParseTypeRef(%q) expected error", in)
        }
    }
}
