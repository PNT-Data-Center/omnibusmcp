// Package jsonx decodes API responses tolerantly. Product APIs change
// between versions (a number becomes a string, 0/1 becomes true/false); a
// single changed field must not break a whole diagnostic tool.
//
// Decode first tries encoding/json. Only if that fails it decodes leniently:
// compatible values are converted, incompatible ones are left at their zero
// value, unknown fields are ignored. Each lenient decode is logged, as it
// signals an API change worth a look (see README, "Zgodność wersji").
package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

var (
	loggedMu sync.Mutex
	logged   = map[string]bool{}
)

// Decode unmarshals data into v; label identifies the source in logs.
// It fails only when data is not JSON at all.
func Decode(label string, data []byte, v any) error {
	strictErr := json.Unmarshal(data, v)
	if strictErr == nil {
		return nil
	}
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return strictErr // not JSON: report the original error
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("jsonx: Decode needs a non-nil pointer, got %T", v)
	}
	target := rv.Elem()
	target.Set(reflect.Zero(target.Type()))
	assign(target, raw)
	logOnce(label, strictErr)
	return nil
}

func logOnce(label string, err error) {
	loggedMu.Lock()
	defer loggedMu.Unlock()
	if logged[label] {
		return
	}
	logged[label] = true
	slog.Warn("lenient JSON decoding used: the API format differs from the tested one", "source", label, "detail", err.Error())
}

var (
	rawMessageType = reflect.TypeOf(json.RawMessage(nil))
	unmarshalerTyp = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
)

// assign stores src (decoded with UseNumber) into dst, converting where
// sensible and leaving dst at its zero value otherwise.
func assign(dst reflect.Value, src any) {
	if src == nil {
		return
	}
	t := dst.Type()
	if t == rawMessageType {
		if b, err := json.Marshal(src); err == nil {
			dst.SetBytes(b)
		}
		return
	}
	if dst.CanAddr() && reflect.PointerTo(t).Implements(unmarshalerTyp) {
		if b, err := json.Marshal(src); err == nil {
			_ = dst.Addr().Interface().(json.Unmarshaler).UnmarshalJSON(b)
		}
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		p := reflect.New(t.Elem())
		assign(p.Elem(), src)
		dst.Set(p)
	case reflect.Interface:
		dst.Set(reflect.ValueOf(plain(src)))
	case reflect.String:
		dst.SetString(toString(src))
	case reflect.Bool:
		dst.SetBool(toBool(src))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if f, ok := toFloat(src); ok {
			dst.SetInt(int64(f))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if f, ok := toFloat(src); ok && f >= 0 {
			dst.SetUint(uint64(f))
		}
	case reflect.Float32, reflect.Float64:
		if f, ok := toFloat(src); ok {
			dst.SetFloat(f)
		}
	case reflect.Slice:
		items, ok := src.([]any)
		if !ok {
			items = []any{src} // a scalar where a list was expected
		}
		s := reflect.MakeSlice(t, len(items), len(items))
		for i, it := range items {
			assign(s.Index(i), it)
		}
		dst.Set(s)
	case reflect.Map:
		m, ok := src.(map[string]any)
		if !ok || t.Key().Kind() != reflect.String {
			return
		}
		out := reflect.MakeMapWithSize(t, len(m))
		for k, v := range m {
			ev := reflect.New(t.Elem()).Elem()
			assign(ev, v)
			out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), ev)
		}
		dst.Set(out)
	case reflect.Struct:
		m, ok := src.(map[string]any)
		if !ok {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			if tag, ok := f.Tag.Lookup("json"); ok {
				tagName, _, _ := strings.Cut(tag, ",")
				if tagName == "-" {
					continue
				}
				if tagName != "" {
					name = tagName
				}
			}
			if v, ok := lookup(m, name); ok {
				assign(dst.Field(i), v)
			}
		}
	}
}

// lookup finds a key exactly, then case-insensitively (as encoding/json).
func lookup(m map[string]any, name string) (any, bool) {
	if v, ok := m[name]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, false
}

// plain converts json.Number to float64 for interface targets, matching
// what encoding/json produces without UseNumber.
func plain(v any) any {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case []any:
		for i := range x {
			x[i] = plain(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = plain(x[k])
		}
		return x
	}
	return v
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case []any:
		parts := make([]string, len(x))
		for i, it := range x {
			parts[i] = toString(it)
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func toBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}
