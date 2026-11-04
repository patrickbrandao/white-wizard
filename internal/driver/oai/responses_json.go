package oai

import (
	"bytes"
	"encoding/json"
)

// decodeOrdered decodes a JSON value keeping the key order of objects (Obj),
// numbers as written (json.Number) and arrays as []any. A value that does
// not decode is returned as raw JSON.
func decodeOrdered(raw []byte) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := orderedValue(dec)
	if err != nil {
		return json.RawMessage(raw)
	}
	return v
}

func orderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			arr := []any{}
			for dec.More() {
				v, err := orderedValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err = dec.Token()
			return arr, err
		}
		o := Obj{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			v, err := orderedValue(dec)
			if err != nil {
				return nil, err
			}
			o = append(o, Field{k, v})
		}
		_, err = dec.Token()
		return o, err
	}
	return tok, nil
}

// echoTool is one element of tools as the response repeats it. A function
// comes out flat with type, description, name, output_schema, parameters
// and strict (function_call_stream). strict omitted means strict when the
// schema fits the Structured Outputs rules, with the schema normalized
// (additionalProperties: false and every key required,
// strict_default_no_additional); a schema that does not fit keeps strict:
// false (not conferred). Other tools are echoed as sent (not conferred).
func echoTool(raw json.RawMessage) any {
	v := decodeOrdered(raw)
	o, ok := v.(Obj)
	if !ok {
		return v
	}
	if typ, _ := o.Get("type"); typ != "function" {
		return o
	}
	name, _ := o.Get("name")
	desc, _ := o.Get("description")
	params, _ := o.Get("parameters")
	strict, has := o.Get("strict")
	if !has || strict == nil {
		if norm, fits := strictSchema(params); fits {
			params, strict = norm, true
		} else {
			strict = false
		}
	}
	return O("type", "function", "description", desc, "name", name, "output_schema", nil, "parameters", params, "strict", strict)
}

// echoFormat is text.format as the response repeats it: json_schema comes
// out as type, description (null when absent), name, schema, strict
// (echo_fields; strict absent is not conferred and stays absent); the
// other formats as sent.
func echoFormat(raw []byte) any {
	v := decodeOrdered(raw)
	o, ok := v.(Obj)
	if !ok {
		return v
	}
	if typ, _ := o.Get("type"); typ != "json_schema" {
		return o
	}
	desc, _ := o.Get("description")
	name, _ := o.Get("name")
	schema, _ := o.Get("schema")
	out := O("type", "json_schema", "description", desc, "name", name, "schema", schema)
	if strict, has := o.Get("strict"); has {
		out = append(out, Field{"strict", strict})
	}
	return out
}

// strictSchema returns s normalized for strict mode and whether it fits:
// every object schema gets additionalProperties: false, appended after its
// own keys, and a required list with every property, in place when it had
// one and appended otherwise. An object that allows additional properties
// does not fit (strict_default_open_schema).
func strictSchema(s any) (any, bool) {
	o, ok := s.(Obj)
	if !ok {
		return s, true
	}
	out := make(Obj, 0, len(o)+2)
	for _, f := range o {
		val := f.Val
		switch f.Key {
		case "properties", "$defs", "definitions":
			if props, isObj := val.(Obj); isObj {
				np := make(Obj, 0, len(props))
				for _, p := range props {
					n, fits := strictSchema(p.Val)
					if !fits {
						return s, false
					}
					np = append(np, Field{p.Key, n})
				}
				val = np
			}
		case "items":
			n, fits := strictSchema(val)
			if !fits {
				return s, false
			}
			val = n
		case "anyOf":
			if arr, isArr := val.([]any); isArr {
				na := make([]any, 0, len(arr))
				for _, x := range arr {
					n, fits := strictSchema(x)
					if !fits {
						return s, false
					}
					na = append(na, n)
				}
				val = na
			}
		}
		out = append(out, Field{f.Key, val})
	}
	if typ, _ := o.Get("type"); typ != "object" {
		return out, true
	}
	if ap, has := o.Get("additionalProperties"); has && ap != false {
		return s, false
	} else if !has {
		out = append(out, Field{"additionalProperties", false})
	}
	var keys []any
	if props, isObj := o.Get("properties"); isObj {
		if p, ok := props.(Obj); ok {
			for _, f := range p {
				keys = append(keys, f.Key)
			}
		}
	}
	if keys == nil {
		keys = []any{}
	}
	if _, has := o.Get("required"); has {
		// A partial list is completed in place (strict_default_partial_required).
		out.Set("required", keys)
		return out, true
	}
	return append(out, Field{"required", keys}), true
}
