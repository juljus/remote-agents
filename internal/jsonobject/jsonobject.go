// Package jsonobject keeps a JSON object in its key order with each value as
// read, so a program rewrites only the keys it sets in a file another
// program owns.
package jsonobject

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Object is a JSON object in its key order.
type Object []member

type member struct {
	key   string
	value json.RawMessage
}

// Parse reads exactly one JSON object; duplicate keys and data after it are errors.
func Parse(data []byte) (Object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o Object
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("object key %v is not a string", tok)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if _, dup := o.Get(key); dup {
			return nil, fmt.Errorf("duplicate key %s", key)
		}
		o = append(o, member{key, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the JSON object")
	}
	return o, nil
}

func (o Object) Get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}

// With is o with key set to value, in place if present, else last.
func (o Object) With(key string, value json.RawMessage) Object {
	out := make(Object, 0, len(o)+1)
	set := false
	for _, m := range o {
		if m.key == key {
			m.value, set = value, true
		}
		out = append(out, m)
	}
	if !set {
		out = append(out, member{key, value})
	}
	return out
}

// Without is o without key.
func (o Object) Without(key string) Object {
	out := make(Object, 0, len(o))
	for _, m := range o {
		if m.key != key {
			out = append(out, m)
		}
	}
	return out
}

// Marshal writes o compactly, or indented by indent when it is not empty.
func (o Object) Marshal(indent string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeKey(&buf, m.key); err != nil {
			return nil, err
		}
		buf.Write(m.value)
	}
	buf.WriteByte('}')
	var out bytes.Buffer
	if indent == "" {
		if err := json.Compact(&out, buf.Bytes()); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}
	if err := json.Indent(&out, buf.Bytes(), "", indent); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// writeKey writes key as a JSON string followed by a colon, escaping as JavaScript's JSON.stringify does.
func writeKey(buf *bytes.Buffer, key string) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(key); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1)
	buf.WriteByte(':')
	return nil
}
