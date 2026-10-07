package payment

import (
	"reflect"
	"strings"
	"testing"
)

func TestNewOrderCodeShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		c, err := NewOrderCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != 10 || !strings.HasPrefix(c, OrderCodePrefix) || !orderCodeRe.MatchString(c) {
			t.Fatalf("bad code %q", c)
		}
		if strings.ContainsAny(c[2:], "01ILOU") {
			t.Fatalf("ambiguous character in %q", c)
		}
	}
}

func TestOrderCodeCandidates(t *testing.T) {
	cases := []struct {
		name, detected, content string
		want                    []string
	}{
		{"sepay detected wins", "KV7K9Q2MXA", "chuyen tien", []string{"KV7K9Q2MXA"}},
		{"plain content", "", "KV7K9Q2MXA chuyen tien", []string{"KV7K9Q2MXA"}},
		{"lower case", "", "kv7k9q2mxa ck", []string{"KV7K9Q2MXA"}},
		{"buyer split the code", "", "ck KV7K9 Q2MXA", []string{"KV7K9Q2MXA"}},
		{"dashes and dots", "", "KV-7K9Q.2MXA", []string{"KV7K9Q2MXA"}},
		{"no code", "", "NGUYEN VAN A chuyen tien", nil},
		{"ambiguous chars are not a code", "", "KV0000OOOO", nil},
		// Gluing words can invent a code; the real one, written as typed,
		// must still be tried first.
		{"real code ranks above a glued false one", "",
			"NGUYEN K VAN THANH CK KV7K9Q2MXA",
			[]string{"KV7K9Q2MXA", "KVANTHANHC"}},
		{"detected and content agree", "KV7K9Q2MXA", "KV7K9Q2MXA", []string{"KV7K9Q2MXA"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := OrderCodeCandidates(tc.detected, tc.content)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestQRURLEncodesAmountAndContent(t *testing.T) {
	got := qrURL("https://vietqr.app/img", "0123456789", "Vietcombank", 199000, "KV7K9Q2MXA")
	want := "https://vietqr.app/img?acc=0123456789&amount=199000&bank=Vietcombank&des=KV7K9Q2MXA&template=compact"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if qrURL("https://vietqr.app/img", "", "Vietcombank", 1, "KV") != "" {
		t.Fatal("missing account must not produce a QR")
	}
}
