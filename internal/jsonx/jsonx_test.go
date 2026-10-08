package jsonx

import (
	"encoding/json"
	"strings"
	"testing"
)

type inner struct {
	Total int64 `json:"total"`
}

type sample struct {
	Name     string            `json:"name"`
	CPU      float64           `json:"cpu"`
	VMID     int               `json:"vmid"`
	Template int               `json:"template"`
	Secure   bool              `json:"secureboot"`
	Load     []string          `json:"loadavg"`
	LoadF    []float64         `json:"loadf"`
	Mem      inner             `json:"memory"`
	Ptr      *inner            `json:"ptr"`
	Labels   map[string]string `json:"labels"`
	Any      any               `json:"any"`
	Raw      json.RawMessage   `json:"raw"`
	Skip     string            `json:"-"`
	Tags     []string          `json:"tags"`
}

func TestStrictPathUnchanged(t *testing.T) {
	var s sample
	if err := Decode("t", []byte(`{"name":"a","cpu":0.5,"vmid":100,"loadavg":["0.1"],"memory":{"total":5}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Name != "a" || s.CPU != 0.5 || s.VMID != 100 || s.Mem.Total != 5 {
		t.Fatalf("%+v", s)
	}
}

func TestLenientConversions(t *testing.T) {
	// Every field has a different type than declared: numbers as strings,
	// 0/1 as bool, floats in a string list, string list as floats, a scalar
	// where a list is expected, mixed label values.
	data := `{"name":42,"cpu":"0.25","vmid":"101","template":true,"secureboot":1,
	  "loadavg":[0.1,0.2,0.3],"loadf":["1.5","2"],"memory":{"total":"1024"},"ptr":{"total":7},
	  "labels":{"a":1,"b":"x"},"any":{"n":3},"raw":[1,2],"Skip":"no","tags":"single","unknown":{"deep":1}}`
	var s sample
	if err := Decode("test-source", []byte(data), &s); err != nil {
		t.Fatal(err)
	}
	checks := map[string]bool{
		"name":       s.Name == "42",
		"cpu":        s.CPU == 0.25,
		"vmid":       s.VMID == 101,
		"template":   s.Template == 1,
		"secureboot": s.Secure,
		"loadavg":    strings.Join(s.Load, " ") == "0.1 0.2 0.3",
		"loadf":      len(s.LoadF) == 2 && s.LoadF[0] == 1.5,
		"memory":     s.Mem.Total == 1024,
		"ptr":        s.Ptr != nil && s.Ptr.Total == 7,
		"labels":     s.Labels["a"] == "1" && s.Labels["b"] == "x",
		"any":        s.Any.(map[string]any)["n"] == 3.0,
		"raw":        string(s.Raw) == "[1,2]",
		"skip":       s.Skip == "",
		"tags":       len(s.Tags) == 1 && s.Tags[0] == "single",
	}
	for k, ok := range checks {
		if !ok {
			t.Errorf("%s not converted: %+v", k, s)
		}
	}
}

func TestIncompatibleFieldsZeroed(t *testing.T) {
	var s sample
	if err := Decode("t", []byte(`{"name":"ok","vmid":"abc","memory":"not-an-object","loadavg":{"x":1}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Name != "ok" || s.VMID != 0 || s.Mem.Total != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestListOfStructsAndInvalidJSON(t *testing.T) {
	var list []sample
	if err := Decode("t", []byte(`[{"vmid":"1"},{"vmid":2}]`), &list); err != nil || len(list) != 2 || list[0].VMID != 1 {
		t.Fatalf("%v %+v", err, list)
	}
	if err := Decode("t", []byte(`not json`), &list); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

type custom struct{ V string }

func (c *custom) UnmarshalJSON(b []byte) error { c.V = "custom:" + string(b); return nil }

func TestCustomUnmarshalerRespected(t *testing.T) {
	var s struct {
		C   custom `json:"c"`
		Bad int    `json:"bad"`
	}
	if err := Decode("t", []byte(`{"c":5,"bad":"x"}`), &s); err != nil || s.C.V != "custom:5" {
		t.Fatalf("%v %+v", err, s)
	}
}
