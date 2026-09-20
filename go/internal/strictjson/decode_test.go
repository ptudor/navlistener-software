package strictjson

import "testing"

func TestStrictDecode(t *testing.T) {
	for _, input := range []string{`{"a":1,"a":2}`, `{"a":[{"b":1,"b":2}]}`, `{"a":1} {}`, `{"unknown":1}`, `{"a":`} {
		var v struct {
			A any `json:"a"`
		}
		if Decode([]byte(input), &v) == nil {
			t.Errorf("accepted %s", input)
		}
	}
	var v struct {
		A int `json:"a"`
	}
	if err := Decode([]byte(`{"a":1}`), &v); err != nil || v.A != 1 {
		t.Fatal(err)
	}
}
