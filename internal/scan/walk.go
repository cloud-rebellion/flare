package scan

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Struct scrubs every string and json.RawMessage reachable from v, in place,
// choosing the rule set per field from its json tag.
//
// Hand-picked field lists are how this package's guarantee kept getting broken.
// The function written to close "unscrubbed metric names" scrubbed Name and
// left Kind raw beside it; scrubEventsForMCP scrubbed five fields and omitted
// Level, Platform, TraceID and SpanID; get_trace scrubbed the span name and
// passed Kind, Status, SpanID and ParentSpanID through. Every one of those
// fields is client-written free text with no enum check anywhere upstream.
//
// So the MCP boundary scrubs by STRUCTURE instead. A field added to a response
// type next year is covered the day it is added, without anyone remembering.
//
// v must be a pointer (or a slice/map of pointers or values) for the rewrite to
// be visible to the caller.
func Struct(v any) {
	if v == nil {
		return
	}
	walk(reflect.ValueOf(v), "")
}

var (
	rawMessageType = reflect.TypeOf(json.RawMessage(nil))
	pairsType      = reflect.TypeOf([]KeyValue(nil))
)

func walk(v reflect.Value, field string) {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			walk(v.Elem(), field)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if t.Field(i).PkgPath != "" {
				continue // unexported
			}
			walk(v.Field(i), jsonName(t.Field(i)))
		}
	case reflect.Slice:
		if v.Type() == rawMessageType {
			if v.CanSet() && v.Len() > 0 {
				v.SetBytes(JSON(v.Bytes()))
			}
			return
		}
		if v.Type() == pairsType {
			Pairs(v.Interface().([]KeyValue))
			return
		}
		for i := 0; i < v.Len(); i++ {
			walk(v.Index(i), field)
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walk(v.Index(i), field)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			val := v.MapIndex(k)
			name := field
			if k.Kind() == reflect.String {
				name = k.String()
			}
			// Map values are not addressable, so they are rebuilt rather than
			// mutated. An `any` value has to be unwrapped first or every leaf
			// would come back as an unaddressable interface.
			nv := rewritten(val, name)
			if nv.IsValid() {
				v.SetMapIndex(k, nv)
			}
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(Field(field, v.String()))
		}
	}
}

// rewritten returns a scrubbed copy of a map value, or the zero Value when
// nothing needed rewriting.
func rewritten(v reflect.Value, field string) reflect.Value {
	if v.Kind() == reflect.Interface && !v.IsNil() {
		inner := rewritten(v.Elem(), field)
		if !inner.IsValid() {
			return reflect.Value{}
		}
		return inner
	}
	switch v.Kind() {
	case reflect.String:
		out := Field(field, v.String())
		if out == v.String() {
			return reflect.Value{}
		}
		nv := reflect.New(v.Type()).Elem()
		nv.SetString(out)
		return nv
	case reflect.Ptr, reflect.Struct, reflect.Slice, reflect.Array, reflect.Map:
		// Composite values behind an interface are addressable through a copy.
		cp := reflect.New(v.Type()).Elem()
		cp.Set(v)
		walk(cp, field)
		return cp
	}
	return reflect.Value{}
}

// jsonName is the wire name of a struct field, which is what Field's policy is
// keyed on. Falls back to the Go name lowercased so an untagged field is still
// classified rather than silently treated as prose.
func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	if tag != "" {
		return tag
	}
	return strings.ToLower(f.Name)
}
