package jsonobject

import (
	"encoding/json"
	"testing"
)

// Laid out as JavaScript's JSON.stringify(v, null, 2) lays it out.
const indented = `{
  "numStartups": 12,
  "tips": {
    "a<b": 1.50,
    "empty": {},
    "list": []
  },
  "oauthAccount": {
    "accountUuid": "old"
  },
  "text": "é & <x>"
}`

func TestObjectRewritesOnlyTheKeySet(t *testing.T) {
	o, err := Parse([]byte(indented))
	if err != nil {
		t.Fatal(err)
	}
	same, err := o.Marshal("  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(same) != indented {
		t.Fatalf("round trip changed the file:\n%s", same)
	}
	changed, err := o.With("oauthAccount", json.RawMessage(`{"accountUuid":"new"}`)).Marshal("  ")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "numStartups": 12,
  "tips": {
    "a<b": 1.50,
    "empty": {},
    "list": []
  },
  "oauthAccount": {
    "accountUuid": "new"
  },
  "text": "é & <x>"
}`
	if string(changed) != want {
		t.Fatalf("got:\n%s", changed)
	}
	compact, err := o.With("added", json.RawMessage(`true`)).Marshal("")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"numStartups":12,"tips":{"a<b":1.50,"empty":{},"list":[]},"oauthAccount":{"accountUuid":"old"},"text":"é & <x>","added":true}`; string(compact) != want {
		t.Fatalf("compact:\n%s", compact)
	}
}

func TestParseObjectRefusesWhatIsNotOneObject(t *testing.T) {
	for _, bad := range []string{`[]`, `{"a":1}{}`, `{"a":1} x`, `{"a":1}}`, `{"a":1,"a":2}`, `{"a":}`, ``} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestWithoutDropsOnlyItsKey(t *testing.T) {
	o, err := Parse([]byte(`{"a":1,"b":2,"c":3}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := o.Without("b").Without("x").Marshal("")
	if err != nil || string(got) != `{"a":1,"c":3}` {
		t.Fatalf("%s, %v", got, err)
	}
}
