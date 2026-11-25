package objstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/acme/ontology/internal/spec"
)

// ErrInvalidValue is returned when a value does not match its property type.
var ErrInvalidValue = errors.New("invalid property value")

func isJSONType(t spec.TypeRef) bool {
	switch t.Base {
	case spec.TGeoPoint, spec.TArray, spec.TStruct:
		return true
	}
	return false
}

// placeholder returns the SQL parameter expression for a property value.
// Values whose Go representation is a string are cast through text to avoid driver ambiguity.
func placeholder(p *spec.Property, n int) string {
	switch {
	case p.Type.Base == spec.TDecimal:
		return fmt.Sprintf("$%d::text::numeric", n)
	case p.Type.Base == spec.TDate:
		return fmt.Sprintf("$%d::text::date", n)
	case p.Type.Base == spec.TTimestamp:
		return fmt.Sprintf("$%d::timestamptz", n)
	case isJSONType(p.Type):
		return fmt.Sprintf("$%d::text::jsonb", n)
	}
	return fmt.Sprintf("$%d", n)
}

// selectExpr returns the column expression used when reading a property.
func selectExpr(c column) string {
	switch {
	case c.prop.Type.Base == spec.TDecimal, c.prop.Type.Base == spec.TDate, isJSONType(c.prop.Type):
		return ident(c.name) + "::text"
	}
	return ident(c.name)
}

func invalid(p *spec.Property, v any, want string) error {
	return fmt.Errorf("%w: property %q expects %s, got %T", ErrInvalidValue, p.APIName, want, v)
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x), true
		}
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	if n, ok := toInt64(v); ok {
		return float64(n), true
	}
	return 0, false
}

// encodeValue converts an API value into a driver parameter for the property's column.
func encodeValue(p *spec.Property, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch p.Type.Base {
	case spec.TString, spec.TAttachment:
		s, ok := v.(string)
		if !ok {
			return nil, invalid(p, v, "string")
		}
		return s, nil
	case spec.TInteger:
		n, ok := toInt64(v)
		if !ok || n < math.MinInt32 || n > math.MaxInt32 {
			return nil, invalid(p, v, "32-bit integer")
		}
		return n, nil
	case spec.TLong:
		n, ok := toInt64(v)
		if !ok {
			return nil, invalid(p, v, "integer")
		}
		return n, nil
	case spec.TDouble:
		f, ok := toFloat64(v)
		if !ok {
			return nil, invalid(p, v, "number")
		}
		return f, nil
	case spec.TBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, invalid(p, v, "boolean")
		}
		return b, nil
	case spec.TDecimal:
		switch x := v.(type) {
		case string:
			if _, ok := new(big.Rat).SetString(x); !ok {
				return nil, invalid(p, v, "decimal string")
			}
			return x, nil
		case json.Number:
			return x.String(), nil
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64), nil
		}
		if n, ok := toInt64(v); ok {
			return strconv.FormatInt(n, 10), nil
		}
		return nil, invalid(p, v, "decimal")
	case spec.TDate:
		switch x := v.(type) {
		case string:
			if _, err := time.Parse(time.DateOnly, x); err != nil {
				return nil, invalid(p, v, "date (YYYY-MM-DD)")
			}
			return x, nil
		case time.Time:
			return x.UTC().Format(time.DateOnly), nil
		}
		return nil, invalid(p, v, "date")
	case spec.TTimestamp:
		switch x := v.(type) {
		case time.Time:
			return x.UTC(), nil
		case string:
			t, err := time.Parse(time.RFC3339Nano, x)
			if err != nil {
				return nil, invalid(p, v, "RFC 3339 timestamp")
			}
			return t.UTC(), nil
		}
		return nil, invalid(p, v, "timestamp")
	case spec.TGeoPoint, spec.TArray, spec.TStruct:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("%w: property %q: %v", ErrInvalidValue, p.APIName, err)
		}
		return string(b), nil
	}
	return nil, fmt.Errorf("%w: property %q of type %s is not stored", ErrInvalidValue, p.APIName, p.Type)
}

// decodeValue normalizes a value read from PostgreSQL.
func decodeValue(p *spec.Property, v any) any {
	if v == nil {
		return nil
	}
	if isJSONType(p.Type) {
		if s, ok := v.(string); ok {
			var out any
			if err := json.Unmarshal([]byte(s), &out); err == nil {
				return out
			}
		}
		return v
	}
	switch x := v.(type) {
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case float32:
		return float64(x)
	case time.Time:
		return x.UTC()
	}
	return v
}

// pkToValue converts a primary key string into a value of the property's type.
func pkToValue(p *spec.Property, pk string) (any, error) {
	switch p.Type.Base {
	case spec.TInteger, spec.TLong:
		n, err := strconv.ParseInt(pk, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: primary key %q is not an integer", ErrInvalidValue, pk)
		}
		return n, nil
	}
	return pk, nil
}

// pkString renders a primary key value as the canonical _pk string.
func pkString(v any) string {
	if n, ok := toInt64(v); ok {
		return strconv.FormatInt(n, 10)
	}
	return fmt.Sprint(v)
}
