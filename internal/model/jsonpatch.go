package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// maxPatchOps bounds one config edit.
const maxPatchOps = 200

// ApplyPatch applies RFC 6902 operations (add, remove, replace, move, copy,
// test) to a
// config and decodes the result strictly. Errors are validation_failed with
// the failing operation's path.
func ApplyPatch(c Config, ops []PatchOp) (Config, error) {
	if len(ops) == 0 {
		return Config{}, patchErr("", "empty_patch", "A config change needs at least one operation")
	}
	if len(ops) > maxPatchOps {
		return Config{}, patchErr("", "too_many", fmt.Sprintf("At most %d operations", maxPatchOps))
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return Config{}, err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Config{}, err
	}
	for i, op := range ops {
		if doc, err = applyOp(doc, op); err != nil {
			if e, ok := apperr.As(err); ok && len(e.Details) > 0 {
				e.Details[0].Index = &i
			}
			return Config{}, err
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return Config{}, err
	}
	var next Config
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		return Config{}, patchErr("", "invalid_config", "The patched config does not decode: "+err.Error())
	}
	return next, nil
}

func patchErr(path, reason, msg string) error {
	return apperr.New(apperr.ValidationFailed, msg, apperr.Detail{Path: path, Reason: reason})
}

func parsePointer(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, patchErr(p, "invalid_pointer", "A path is a JSON pointer starting with /")
	}
	parts := strings.Split(p[1:], "/")
	for i, s := range parts {
		parts[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
	}
	return parts, nil
}

func applyOp(doc any, op PatchOp) (any, error) {
	path, err := parsePointer(op.Path)
	if err != nil {
		return nil, err
	}
	if op.Op == "move" || op.Op == "copy" {
		from, err := parsePointer(op.From)
		if err != nil || len(from) == 0 {
			return nil, patchErr(op.From, "invalid_pointer", op.Op+" needs a from path below the root")
		}
		if op.Op == "move" && strings.HasPrefix(op.Path+"/", op.From+"/") {
			return nil, patchErr(op.Path, "invalid_pointer", "Cannot move a value into itself")
		}
		v, err := walk(doc, from, op.From)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if op.Op == "move" {
			if doc, err = applyOp(doc, PatchOp{Op: "remove", Path: op.From}); err != nil {
				return nil, err
			}
		}
		return applyOp(doc, PatchOp{Op: "add", Path: op.Path, Value: raw})
	}
	var val any
	if op.Op != "remove" {
		if len(op.Value) == 0 {
			return nil, patchErr(op.Path, "value_required", op.Op+" needs a value")
		}
		if err := json.Unmarshal(op.Value, &val); err != nil {
			return nil, patchErr(op.Path, "invalid_value", "Invalid JSON value")
		}
	}
	if len(path) == 0 {
		switch op.Op {
		case "replace":
			return val, nil
		case "test":
			if !reflect.DeepEqual(doc, val) {
				return nil, patchErr(op.Path, "test_failed", "Test failed at the document root")
			}
			return doc, nil
		}
		return nil, patchErr(op.Path, "invalid_pointer", op.Op+" needs a path below the root")
	}
	parent, err := walk(doc, path[:len(path)-1], op.Path)
	if err != nil {
		return nil, err
	}
	last := path[len(path)-1]
	switch c := parent.(type) {
	case map[string]any:
		cur, exists := c[last]
		switch op.Op {
		case "add":
			c[last] = val
		case "replace", "remove":
			if !exists {
				return nil, patchErr(op.Path, "not_found", "Nothing at "+op.Path)
			}
			if op.Op == "remove" {
				delete(c, last)
			} else {
				c[last] = val
			}
		case "test":
			if !exists || !reflect.DeepEqual(cur, val) {
				return nil, patchErr(op.Path, "test_failed", "Test failed at "+op.Path)
			}
		default:
			return nil, patchErr(op.Path, "unsupported_op", "Supported ops: add, remove, replace, move, copy, test")
		}
		return doc, nil
	case []any:
		if op.Op == "add" && last == "-" {
			return setAt(doc, path[:len(path)-1], append(c, val))
		}
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i > len(c) || (i == len(c) && op.Op != "add") {
			return nil, patchErr(op.Path, "not_found", "No index "+last+" at "+op.Path)
		}
		var next []any
		switch op.Op {
		case "add":
			next = append(append(append([]any{}, c[:i]...), val), c[i:]...)
		case "remove":
			next = append(append([]any{}, c[:i]...), c[i+1:]...)
		case "replace":
			next = append([]any{}, c...)
			next[i] = val
		case "test":
			if !reflect.DeepEqual(c[i], val) {
				return nil, patchErr(op.Path, "test_failed", "Test failed at "+op.Path)
			}
			return doc, nil
		default:
			return nil, patchErr(op.Path, "unsupported_op", "Supported ops: add, remove, replace, move, copy, test")
		}
		return setAt(doc, path[:len(path)-1], next)
	}
	return nil, patchErr(op.Path, "not_found", "The parent of "+op.Path+" is not an object or array")
}

func walk(doc any, path []string, full string) (any, error) {
	cur := doc
	for _, p := range path {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[p]
			if !ok {
				return nil, patchErr(full, "not_found", "Nothing at "+full)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(c) {
				return nil, patchErr(full, "not_found", "Nothing at "+full)
			}
			cur = c[i]
		default:
			return nil, patchErr(full, "not_found", "Nothing at "+full)
		}
	}
	return cur, nil
}

// setAt replaces the value at path (whose parent exists) and returns doc.
func setAt(doc any, path []string, v any) (any, error) {
	if len(path) == 0 {
		return v, nil
	}
	parent, err := walk(doc, path[:len(path)-1], "")
	if err != nil {
		return nil, err
	}
	last := path[len(path)-1]
	switch c := parent.(type) {
	case map[string]any:
		c[last] = v
	case []any:
		i, _ := strconv.Atoi(last)
		c[i] = v
	}
	return doc, nil
}

// DiffConfigs returns operations that turn a into b: objects are compared
// key by key, anything else (arrays included) is replaced whole.
func DiffConfigs(a, b Config) ([]PatchOp, error) {
	var da, db any
	for _, x := range []struct {
		c   Config
		out *any
	}{{a, &da}, {b, &db}} {
		raw, err := json.Marshal(x.c)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, x.out); err != nil {
			return nil, err
		}
	}
	ops := []PatchOp{}
	err := diff("", da, db, &ops)
	return ops, err
}

func escape(s string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(s) }

func diff(path string, a, b any, ops *[]PatchOp) error {
	ma, okA := a.(map[string]any)
	mb, okB := b.(map[string]any)
	if okA && okB {
		keys := map[string]bool{}
		for k := range ma {
			keys[k] = true
		}
		for k := range mb {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			p := path + "/" + escape(k)
			va, inA := ma[k]
			vb, inB := mb[k]
			switch {
			case !inB:
				*ops = append(*ops, PatchOp{Op: "remove", Path: p})
			case !inA:
				raw, err := json.Marshal(vb)
				if err != nil {
					return err
				}
				*ops = append(*ops, PatchOp{Op: "add", Path: p, Value: raw})
			default:
				if err := diff(p, va, vb, ops); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if reflect.DeepEqual(a, b) {
		return nil
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	*ops = append(*ops, PatchOp{Op: "replace", Path: path, Value: raw})
	return nil
}
