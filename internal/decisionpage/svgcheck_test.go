package decisionpage

import "testing"

func TestValidateSVG(t *testing.T) {
	good := `<svg viewBox="0 0 100 50" role="img" aria-label="x"><rect x="1" y="1" width="10" height="10" fill="none" stroke="currentColor"/><text x="5" y="5">ok</text></svg>`
	if err := ValidateSVG(good); err != nil {
		t.Fatalf("good svg rejected: %v", err)
	}
	bad := []string{
		`<div>not svg</div>`,
		`<svg viewBox="0 0 1 1"><script>alert(1)</script></svg>`,
		`<svg viewBox="0 0 1 1"><style>*{display:none}</style></svg>`,
		`<svg viewBox="0 0 1 1"><foreignObject/></svg>`,
		`<svg viewBox="0 0 1 1"><image href="https://evil"/></svg>`,
		`<svg viewBox="0 0 1 1"><rect onload="alert(1)"/></svg>`,
		`<svg viewBox="0 0 1 1"><a href="javascript:alert(1)"><rect/></a></svg>`,
		`<svg viewBox="0 0 1 1"><use href="https://evil#x"/></svg>`,
		`<svg><rect/></svg>`,
	}
	for i, s := range bad {
		if err := ValidateSVG(s); err == nil {
			t.Errorf("bad svg %d accepted", i)
		}
	}
}

func TestValidateSVGRejectsStyleAttributesAndExternalURLs(t *testing.T) {
	for _, svg := range []string{
		`<svg viewBox="0 0 1 1"><rect style="display:none"/></svg>`,
		`<svg viewBox="0 0 1 1"><path fill="url(https://evil)"/></svg>`,
		`<svg viewBox="0 0 1 1"><rect data-url="javascript:alert(1)"/></svg>`,
	} {
		if err := ValidateSVG(svg); err == nil {
			t.Errorf("unsafe svg accepted: %s", svg)
		}
	}
}
