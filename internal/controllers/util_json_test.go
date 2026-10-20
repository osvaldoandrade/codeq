package controllers

import (
	"encoding/json"
	"testing"
)

const (
	preciseJSONPayload = `{"n":9007199254740993}`
	nullJSONPayload    = `null`
)

func TestCanonicalPayloadPrecisionAndLegacyNormalization(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		preserve bool
		want     string
		invalid  bool
	}{
		{preciseJSONPayload, true, preciseJSONPayload, false},
		{preciseJSONPayload, false, `{"n":9007199254740992}`, false},
		{`[9223372036854775807,0.1234567890123456789,1e100]`, true, `[9223372036854775807,0.1234567890123456789,1e100]`, false},
		{` { "b":false, "a":[1] } `, true, `{"a":[1],"b":false}`, false},
		{nullJSONPayload, false, nullJSONPayload, false},
		{``, false, nullJSONPayload, false},
		{nullJSONPayload, true, ``, true},
		{`{} {}`, true, ``, true},
		{`{`, true, ``, true},
	} {
		got, err := canonicalPayload(json.RawMessage(tc.raw), tc.preserve)
		if (err != nil) != tc.invalid || got != tc.want {
			t.Errorf("raw=%s preserve=%v: got=%s err=%v", tc.raw, tc.preserve, got, err)
		}
	}
}

func FuzzCanonicalAbsolutePayload(f *testing.F) {
	for _, seed := range []string{`{"large":9007199254740993}`, `[0.1234567890123456789,9223372036854775807]`, `true`, `null`, `{`, `{} {}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 16384 {
			t.Skip()
		}
		canonical, err := canonicalPayload(json.RawMessage(raw), true)
		if err != nil {
			return
		}
		if !json.Valid([]byte(canonical)) {
			t.Fatal("decoder returned invalid JSON")
		}
		again, err := canonicalPayload(json.RawMessage(canonical), true)
		if err != nil || again != canonical {
			t.Fatal("normalization is not stable")
		}
	})
}
