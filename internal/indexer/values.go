package indexer

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/acme/ontology/internal/spec"
)

// normalize turns a value read from a source database (or decoded from a Debezium JSON event)
// into what objstore's encoder accepts for the property's base type.
func normalize(base spec.BaseType, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch base {
	case spec.TString, spec.TAttachment:
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		case fmt.Stringer:
			return x.String(), nil
		}
		return fmt.Sprint(v), nil
	case spec.TInteger, spec.TLong:
		return toInt(v)
	case spec.TDouble:
		return toFloat(v)
	case spec.TDecimal:
		switch x := v.(type) {
		case string:
			return x, nil
		case json.Number:
			return x.String(), nil
		}
		if f, err := toFloat(v); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64), nil
		}
		return nil, fmt.Errorf("cannot convert %T to decimal", v)
	case spec.TBoolean:
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			return strconv.ParseBool(x)
		}
		return nil, fmt.Errorf("cannot convert %T to boolean", v)
	case spec.TTimestamp:
		switch x := v.(type) {
		case time.Time:
			return x.UTC(), nil
		case string:
			t, err := time.Parse(time.RFC3339Nano, x)
			if err != nil {
				return nil, fmt.Errorf("timestamp %q: %w", x, err)
			}
			return t.UTC(), nil
		}
		return nil, fmt.Errorf("cannot convert %T to timestamp", v)
	case spec.TDate:
		switch x := v.(type) {
		case time.Time:
			return x.UTC().Format(time.DateOnly), nil
		case string:
			if len(x) >= 10 {
				return x[:10], nil
			}
		}
		if n, err := toInt(v); err == nil { // Debezium io.debezium.time.Date: days since epoch
			return time.Unix(n*86400, 0).UTC().Format(time.DateOnly), nil
		}
		return nil, fmt.Errorf("cannot convert %T to date", v)
	case spec.TGeoPoint, spec.TArray, spec.TStruct:
		if s, ok := v.(string); ok {
			var out any
			if err := json.Unmarshal([]byte(s), &out); err != nil {
				return nil, fmt.Errorf("invalid JSON value: %w", err)
			}
			return out, nil
		}
		return v, nil
	}
	return v, nil
}

func toInt(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x), nil
		}
	case json.Number:
		return x.Int64()
	case string:
		return strconv.ParseInt(x, 10, 64)
	case *big.Int:
		if x.IsInt64() {
			return x.Int64(), nil
		}
	}
	return 0, fmt.Errorf("cannot convert %v (%T) to integer", v, v)
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case json.Number:
		return x.Float64()
	case string:
		return strconv.ParseFloat(x, 64)
	}
	if n, err := toInt(v); err == nil {
		return float64(n), nil
	}
	return 0, fmt.Errorf("cannot convert %v (%T) to number", v, v)
}
