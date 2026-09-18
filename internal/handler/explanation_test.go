package handler

import "testing"

func TestValidateExplanationAcceptsAValidSlide(t *testing.T) {
	in := `{"v":1,"layout":"image_right","bg":{"color":"#12141a"},"overlay":0.45,` +
		`"elements":[{"id":"title","kind":"text","role":"title","text":"Why","style":{"color":"#f2f0eb"}},` +
		`{"id":"media","kind":"image","role":"media","url":"/uploads/img-u1-1-abc.png"}]}`
	out, err := validateExplanation(in)
	if err != nil {
		t.Fatalf("expected the slide to validate, got %v", err)
	}
	if out != in {
		t.Fatalf("expected the document to be stored unchanged")
	}
}

func TestValidateExplanationTreatsBlankAsNoSlide(t *testing.T) {
	for _, in := range []string{"", "   ", "\n"} {
		out, err := validateExplanation(in)
		if err != nil || out != "" {
			t.Fatalf("blank input %q should clear the slide, got (%q, %v)", in, out, err)
		}
	}
}

func TestValidateExplanationRejectsUnsafeValues(t *testing.T) {
	cases := map[string]string{
		"unknown layout":       `{"v":1,"layout":"freeform","elements":[]}`,
		"malformed json":       `{"v":1,"layout":"text",`,
		"overlay out of range": `{"v":1,"layout":"image_full","overlay":1.5,"elements":[]}`,
		// Colours are written into an inline style attribute on the client, so
		// anything that is not a plain hex triple is a CSS injection sink.
		"css function as bg color": `{"v":1,"layout":"text","bg":{"color":"url(javascript:alert(1))"},"elements":[]}`,
		"css function as text color": `{"v":1,"layout":"text","elements":` +
			`[{"id":"t","kind":"text","text":"x","style":{"color":"expression(alert(1))"}}]}`,
		"named color": `{"v":1,"layout":"text","bg":{"color":"red"},"elements":[]}`,
		"javascript image url": `{"v":1,"layout":"image_top","elements":` +
			`[{"id":"m","kind":"image","url":"javascript:alert(1)"}]}`,
		"data image url": `{"v":1,"layout":"image_top","elements":` +
			`[{"id":"m","kind":"image","url":"data:text/html;base64,PHN2Zz4="}]}`,
		"http image url": `{"v":1,"layout":"image_top","elements":` +
			`[{"id":"m","kind":"image","url":"http://example.com/a.png"}]}`,
		"traversal upload url": `{"v":1,"layout":"image_top","elements":` +
			`[{"id":"m","kind":"image","url":"/uploads/../../etc/passwd"}]}`,
		"unknown element kind": `{"v":1,"layout":"text","elements":` +
			`[{"id":"s","kind":"script","text":"x"}]}`,
	}
	for name, in := range cases {
		if _, err := validateExplanation(in); err == nil {
			t.Errorf("%s: expected rejection, got none", name)
		}
	}
}

func TestValidateExplanationRejectsOversizedDocuments(t *testing.T) {
	body := make([]byte, maxExplanationBytes+1)
	for i := range body {
		body[i] = 'a'
	}
	if _, err := validateExplanation(string(body)); err == nil {
		t.Fatal("expected an oversized document to be rejected")
	}
}

func TestIsAllowedMediaURL(t *testing.T) {
	allowed := []string{"/uploads/img-u1-2-abc.png", "https://media.giphy.com/a.gif"}
	for _, u := range allowed {
		if !isAllowedMediaURL(u) {
			t.Errorf("expected %q to be allowed", u)
		}
	}
	refused := []string{"", "//evil.example/a.png", "http://x/a.png", "data:image/png;base64,AA", "/uploads/../secret"}
	for _, u := range refused {
		if isAllowedMediaURL(u) {
			t.Errorf("expected %q to be refused", u)
		}
	}
}
