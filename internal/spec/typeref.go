package spec

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseTypeRef parses "string", "vector(1024)", "array<string>", "decimal(10,2)" ...
func ParseTypeRef(s string) (TypeRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return TypeRef{}, fmt.Errorf("type is required")
	}
	switch {
	case strings.HasPrefix(s, "array<") && strings.HasSuffix(s, ">"):
		elem, err := ParseTypeRef(s[len("array<") : len(s)-1])
		if err != nil {
			return TypeRef{}, fmt.Errorf("array element: %w", err)
		}
		if elem.Base == TArray {
			return TypeRef{}, fmt.Errorf("nested arrays are not supported")
		}
		return TypeRef{Base: TArray, Elem: &elem}, nil
	case strings.HasPrefix(s, "vector(") && strings.HasSuffix(s, ")"):
		n, err := strconv.Atoi(strings.TrimSpace(s[len("vector(") : len(s)-1]))
		if err != nil || n <= 0 || n > 4096 {
			return TypeRef{}, fmt.Errorf("vector dimension must be an integer in 1..4096")
		}
		return TypeRef{Base: TVector, Dim: n}, nil
	case strings.HasPrefix(s, "decimal(") && strings.HasSuffix(s, ")"):
		parts := strings.Split(s[len("decimal("):len(s)-1], ",")
		if len(parts) != 2 {
			return TypeRef{}, fmt.Errorf("decimal must be written decimal(precision,scale)")
		}
		p, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		sc, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || p < 1 || p > 38 || sc < 0 || sc > p {
			return TypeRef{}, fmt.Errorf("decimal precision must be 1..38 and scale 0..precision")
		}
		return TypeRef{Base: TDecimal, Precision: p, Scale: sc}, nil
	}
	switch b := BaseType(s); b {
	case TDecimal:
		return TypeRef{Base: TDecimal, Precision: 38, Scale: 9}, nil
	case TString, TInteger, TLong, TDouble, TBoolean, TDate, TTimestamp, TGeoPoint, TAttachment, TStruct:
		return TypeRef{Base: b}, nil
	case TVector:
		return TypeRef{}, fmt.Errorf("vector requires a dimension, e.g. vector(1024)")
	case TArray:
		return TypeRef{}, fmt.Errorf("array requires an element type, e.g. array<string>")
	}
	return TypeRef{}, fmt.Errorf("unknown type %q", s)
}

func (t TypeRef) String() string {
	switch t.Base {
	case TArray:
		if t.Elem != nil {
			return "array<" + t.Elem.String() + ">"
		}
	case TVector:
		return fmt.Sprintf("vector(%d)", t.Dim)
	case TDecimal:
		return fmt.Sprintf("decimal(%d,%d)", t.Precision, t.Scale)
	}
	return string(t.Base)
}
