package decisionpage

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

var svgAllowedElems = map[string]bool{
	"svg": true, "g": true, "defs": true, "marker": true,
	"rect": true, "circle": true, "ellipse": true, "line": true,
	"polyline": true, "polygon": true, "path": true, "text": true,
	"tspan": true, "title": true, "desc": true,
}

// ValidateSVG accepts only static drawing markup: allowlisted elements, no
// event handlers or style attributes, and references that stay inside the
// SVG fragment. A valid result is safe to wrap in template.HTML.
func ValidateSVG(svg string) error {
	if strings.TrimSpace(svg) == "" {
		return fmt.Errorf("svg is empty")
	}

	dec := xml.NewDecoder(strings.NewReader(svg))
	dec.Strict = true
	depth := 0
	sawRoot := false
	sawViewBox := false
	closedRoot := false

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("svg parse: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if depth == 0 {
				if closedRoot {
					return fmt.Errorf("multiple root elements")
				}
				if name != "svg" {
					return fmt.Errorf("root element is %q, want svg", t.Name.Local)
				}
				sawRoot = true
			}
			if !svgAllowedElems[name] {
				return fmt.Errorf("element %q not allowed", t.Name.Local)
			}
			for _, attr := range t.Attr {
				attrName := strings.ToLower(attr.Name.Local)
				value := strings.TrimSpace(attr.Value)
				lowerValue := strings.ToLower(value)

				if strings.HasPrefix(attrName, "on") || attrName == "style" {
					return fmt.Errorf("unsafe attribute %q", attr.Name.Local)
				}
				if depth == 0 && attrName == "viewbox" {
					if err := validViewBox(value); err != nil {
						return err
					}
					sawViewBox = true
				}
				if attrName == "href" || attrName == "xlink:href" || attr.Name.Space == "http://www.w3.org/1999/xlink" {
					if !strings.HasPrefix(value, "#") {
						return fmt.Errorf("href must be fragment-internal, got %q", value)
					}
				}
				if strings.Contains(lowerValue, "javascript:") {
					return fmt.Errorf("javascript URI in %q", attr.Name.Local)
				}
				if strings.Contains(lowerValue, "url(") && !strings.Contains(lowerValue, "url(#") {
					return fmt.Errorf("external url() reference in %q", attr.Name.Local)
				}
			}
			depth++
		case xml.EndElement:
			if depth == 0 {
				return fmt.Errorf("unexpected closing element %q", t.Name.Local)
			}
			depth--
			if depth == 0 {
				closedRoot = true
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return fmt.Errorf("text outside svg root")
			}
		case xml.Directive, xml.ProcInst:
			return fmt.Errorf("XML directives and processing instructions are not allowed")
		}
	}

	if !sawRoot {
		return fmt.Errorf("no svg root element")
	}
	if depth != 0 {
		return fmt.Errorf("svg root is not closed")
	}
	if !sawViewBox {
		return fmt.Errorf("svg root missing viewBox")
	}
	return nil
}

func validViewBox(value string) error {
	parts := strings.Fields(value)
	if len(parts) != 4 {
		return fmt.Errorf("svg viewBox must contain four numbers")
	}
	for _, part := range parts {
		if _, err := strconv.ParseFloat(part, 64); err != nil {
			return fmt.Errorf("svg viewBox contains non-numeric value %q", part)
		}
	}
	return nil
}
