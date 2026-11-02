package jobs

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
)

// GenerateArgsSchema derives a JSON Schema (2020-12) from an Args struct type.
// Fields without `omitempty` (and not pointers) are required; unknown
// properties are rejected, matching the strict decoder used at run time.
func GenerateArgsSchema(t reflect.Type) json.RawMessage {
	s := schemaFor(t, map[reflect.Type]bool{})
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	b, err := json.Marshal(s)
	if err != nil {
		// Only map/slice/string/bool values are built above; this cannot fail.
		panic(fmt.Sprintf("jobs: marshal schema: %v", err))
	}
	return b
}

var (
	timeType     = reflect.TypeFor[time.Time]()
	durationType = reflect.TypeFor[time.Duration]()
	dateType     = reflect.TypeFor[civil.Date]()
	uuidType     = reflect.TypeFor[uuid.UUID]()
	rawType      = reflect.TypeFor[json.RawMessage]()
)

type schema = map[string]any

func schemaFor(t reflect.Type, seen map[reflect.Type]bool) schema {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case timeType:
		return schema{"type": "string", "format": "date-time"}
	case dateType:
		return schema{"type": "string", "format": "date"}
	case uuidType:
		return schema{"type": "string", "format": "uuid"}
	case rawType:
		return schema{}
	case durationType:
		return schema{"type": "integer", "description": "nanoseconds"}
	}
	switch t.Kind() {
	case reflect.String:
		return schema{"type": "string"}
	case reflect.Bool:
		return schema{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return schema{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return schema{"type": "number"}
	case reflect.Slice, reflect.Array:
		return schema{"type": "array", "items": schemaFor(t.Elem(), seen)}
	case reflect.Map:
		return schema{"type": "object", "additionalProperties": schemaFor(t.Elem(), seen)}
	case reflect.Struct:
		return structSchema(t, seen)
	default:
		return schema{}
	}
}

func structSchema(t reflect.Type, seen map[reflect.Type]bool) schema {
	if seen[t] {
		return schema{"type": "object"} // recursive type: stop expanding
	}
	seen[t] = true
	defer delete(seen, t)

	props := schema{}
	var required []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		props[name] = schemaFor(f.Type, seen)
		if !strings.Contains(opts, "omitempty") && !strings.Contains(opts, "omitzero") && f.Type.Kind() != reflect.Pointer {
			required = append(required, name)
		}
	}
	s := schema{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
