package audit

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// sensitiveKey matches field and map keys that must never reach the audit
// log, whatever their value.
var sensitiveKey = regexp.MustCompile(`(?i)(password|passwd|token|secret|hash|cookie|session|api_?key|credential|authorization|private)`)

// maxDepth bounds recursion into nested values.
const maxDepth = 16

// Snapshot converts v into the JSON stored in before/after: struct fields
// tagged `audit:"-"` and keys that look sensitive are dropped, recursively.
// Non-object values are wrapped as {"value": …}. A nil v yields nil.
func Snapshot(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("audit: snapshot: %w", err)
		}
		v = decoded
	}
	clean := redactValue(reflect.ValueOf(v), 0)
	if clean == nil {
		return nil, nil
	}
	if _, isObj := clean.(map[string]any); !isObj {
		clean = map[string]any{"value": clean}
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return nil, fmt.Errorf("audit: snapshot: %w", err)
	}
	return b, nil
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

func redactValue(v reflect.Value, depth int) any {
	if !v.IsValid() || depth > maxDepth {
		return nil
	}
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	// Leaf types that know their own JSON form (time.Time, uuid.UUID,
	// decimal.Decimal, civil.Date, …).
	if v.Type().Implements(jsonMarshaler) || v.Type().Implements(textMarshaler) ||
		reflect.PointerTo(v.Type()).Implements(jsonMarshaler) {
		return v.Interface()
	}
	switch v.Kind() {
	case reflect.Struct:
		return redactStruct(v, depth)
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return nil
		}
		out := make(map[string]any, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			k := iter.Key().String()
			if sensitiveKey.MatchString(k) {
				continue
			}
			out[k] = redactValue(iter.Value(), depth+1)
		}
		return out
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			// Raw bytes are never useful in an audit record and may be hashes.
			return nil
		}
		out := make([]any, v.Len())
		for i := range out {
			out[i] = redactValue(v.Index(i), depth+1)
		}
		return out
	case reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128:
		return nil
	default:
		return v.Interface()
	}
}

func redactStruct(v reflect.Value, depth int) map[string]any {
	t := v.Type()
	out := make(map[string]any, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() || f.Tag.Get("audit") == "-" {
			continue
		}
		name, omitEmpty, skip := jsonName(f)
		if skip || sensitiveKey.MatchString(name) || sensitiveKey.MatchString(f.Name) {
			continue
		}
		fv := v.Field(i)
		if omitEmpty && fv.IsZero() {
			continue
		}
		if f.Anonymous && fv.Kind() == reflect.Struct && f.Tag.Get("json") == "" {
			for k, val := range redactStruct(fv, depth+1) {
				out[k] = val
			}
			continue
		}
		out[name] = redactValue(fv, depth+1)
	}
	return out
}

func jsonName(f reflect.StructField) (name string, omitEmpty, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	for _, p := range parts[1:] {
		if p == "omitempty" || p == "omitzero" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}
