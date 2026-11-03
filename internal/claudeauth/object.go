package claudeauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// object is a JSON object kept in its key order with each value as read, so
// a rewrite changes only the keys this package sets.
type object []member

type member struct {
	key   string
	value json.RawMessage
}

func parseObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o object
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
		if _, dup := o.get(key); dup {
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

func (o object) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}

// with is o with key set to value, in place if present, else last.
func (o object) with(key string, value json.RawMessage) object {
	out := make(object, 0, len(o)+1)
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

// marshal writes o compactly, or indented by indent when it is not empty.
func (o object) marshal(indent string) ([]byte, error) {
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
