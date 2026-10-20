package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"
)

// Object is a JSON object with its keys in the order the API sent them (a table's columns follow it).
type Object struct {
	Keys []string
	Vals map[string]any
}

// Get is a field, or a nested one by a dotted path (liquidation.price).
func (o *Object) Get(path string) (any, bool) {
	var cur any = o
	for _, k := range strings.Split(path, ".") {
		obj, ok := cur.(*Object)
		if !ok {
			return nil, false
		}
		if cur, ok = obj.Vals[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// Decode reads JSON keeping the objects' key order and every number exactly (json.Number).
func Decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("more than one JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch d := t.(type) {
	case json.Delim:
		if d == '[' {
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token()
			return arr, err
		}
		obj := &Object{Vals: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k := kt.(string)
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			if _, dup := obj.Vals[k]; !dup {
				obj.Keys = append(obj.Keys, k)
			}
			obj.Vals[k] = v
		}
		_, err := dec.Token()
		return obj, err
	default:
		return t, nil
	}
}

// Plain is a value as encoding/json and gojq take it: maps for objects, numbers as int, *big.Int or float64.
func Plain(v any) any {
	switch x := v.(type) {
	case *Object:
		m := make(map[string]any, len(x.Keys))
		for _, k := range x.Keys {
			m[k] = Plain(x.Vals[k])
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Plain(e)
		}
		return out
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		if b, ok := new(big.Int).SetString(string(x), 10); ok {
			return b
		}
		f, _ := x.Float64()
		return f
	default:
		return v
	}
}

// Encode writes a value as JSON, its objects in their own key order and its numbers exactly.
func Encode(w *bytes.Buffer, v any) {
	switch x := v.(type) {
	case *Object:
		w.WriteByte('{')
		for i, k := range x.Keys {
			if i > 0 {
				w.WriteByte(',')
			}
			w.Write(marshal(k))
			w.WriteByte(':')
			Encode(w, x.Vals[k])
		}
		w.WriteByte('}')
	case []any:
		w.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				w.WriteByte(',')
			}
			Encode(w, e)
		}
		w.WriteByte(']')
	case json.Number:
		w.WriteString(string(x))
	default:
		w.Write(marshal(x))
	}
}

// marshal is encoding/json without its HTML escapes (a <, > or & shows as itself).
func marshal(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// Compact is a value as one line of JSON.
func Compact(v any) string {
	var b bytes.Buffer
	Encode(&b, v)
	return b.String()
}
