package output

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRecordAndJSON(t *testing.T) {
	v, err := Decode([]byte(`{"z":"1.10","a":{"b":null,"when_at":1791460800000},"list":["x","y"],"big":123456789012345678901234567890,"html":"<a&b>"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p := &Printer{Out: &out, Format: "table", Zone: time.UTC}
	if err := p.Print(v); err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, kv := range [][2]string{{"z", "1.10"}, {"a.b", "-"}, {"a.when_at", "2026-10-08 12:00:00"}, {"list", "x, y"}, {"big", "123456789012345678901234567890"}, {"html", "<a&b>"}} {
		want.WriteString(kv[0] + strings.Repeat(" ", 11-len(kv[0])) + kv[1] + "\n")
	}
	if out.String() != want.String() {
		t.Fatalf("record:\n%s\nwant:\n%s", out.String(), want.String())
	}
	out.Reset()
	p.Format = "json"
	_ = p.Print(v)
	if !strings.HasPrefix(out.String(), "{\n  \"z\": \"1.10\",\n  \"a\": {") || !strings.Contains(out.String(), `"big": 123456789012345678901234567890`) || !strings.Contains(out.String(), `"<a&b>"`) {
		t.Fatalf("json keeps order, numbers and text:\n%s", out.String())
	}
	out.Reset()
	code, _ := ParseJQ(".big + 1")
	p.JQ = code
	if err := p.Print(v); err != nil || out.String() != "123456789012345678901234567891\n" {
		t.Fatalf("jq on big numbers: %v %q", err, out.String())
	}
}
