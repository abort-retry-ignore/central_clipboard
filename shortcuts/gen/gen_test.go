package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func actions(t *testing.T) []dict {
	t.Helper()
	var out []dict
	for _, a := range workflow()["WFWorkflowActions"].([]any) {
		out = append(out, a.(dict))
	}
	return out
}

func params(a dict) dict { return a["WFWorkflowActionParameters"].(dict) }

func ident(a dict) string {
	return strings.TrimPrefix(a["WFWorkflowActionIdentifier"].(string), "is.workflow.actions.")
}

func TestMenuControlFlowIsBalanced(t *testing.T) {
	var modes []int
	var titles, items []string
	group := ""
	for _, a := range actions(t) {
		if ident(a) != "choosefrommenu" {
			continue
		}
		p := params(a)
		if group == "" {
			group = p["GroupingIdentifier"].(string)
		}
		if p["GroupingIdentifier"] != group {
			t.Fatal("menu actions use different grouping identifiers")
		}
		modes = append(modes, p["WFControlFlowMode"].(int))
		if v, ok := p["WFMenuItemTitle"]; ok {
			titles = append(titles, v.(string))
		}
		if v, ok := p["WFMenuItems"]; ok {
			for _, s := range v.([]any) {
				items = append(items, s.(string))
			}
		}
	}
	if want := []int{0, 1, 1, 2}; len(modes) != 4 || modes[0] != want[0] || modes[1] != want[1] || modes[2] != want[2] || modes[3] != want[3] {
		t.Fatalf("control flow modes = %v, want start, item, item, end", modes)
	}
	if strings.Join(titles, ",") != "Send,Get" || strings.Join(items, ",") != "Send,Get" {
		t.Fatalf("menu items %v, branch titles %v", items, titles)
	}
}

func TestEveryReferencedOutputExistsEarlier(t *testing.T) {
	seen := map[string]bool{}
	var check func(v any)
	check = func(v any) {
		switch v := v.(type) {
		case dict:
			if id, ok := v["OutputUUID"]; ok && !seen[id.(string)] {
				t.Fatalf("output %v referenced before it exists", id)
			}
			for _, e := range v {
				check(e)
			}
		}
	}
	for _, a := range actions(t) {
		p := params(a)
		check(p)
		if id, ok := p["UUID"]; ok {
			seen[id.(string)] = true
		}
	}
}

func TestImportQuestionAsksForTheURLField(t *testing.T) {
	q := workflow()["WFWorkflowImportQuestions"].([]any)[0].(dict)
	first := actions(t)[q["ActionIndex"].(int)]
	if ident(first) != "gettext" || q["ParameterKey"] != "WFTextActionText" {
		t.Fatalf("question points at %s / %v", ident(first), q["ParameterKey"])
	}
	if p := params(first); p["WFTextActionText"] != "" {
		t.Fatal("the URL must be empty in the file; it is entered at import")
	}
}

func TestNoHubAddressOrTokenIsBakedIn(t *testing.T) {
	// Only the actions matter: the import question's own example text mentions an address.
	var buf bytes.Buffer
	if err := writeValue(&buf, workflow()["WFWorkflowActions"], 0); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"http", "://", "?t=", "100."} {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("the actions contain %q", s)
		}
	}
}

func TestOutputIsWellFormedAndStable(t *testing.T) {
	var a, b bytes.Buffer
	writePlist(&a, workflow())
	writePlist(&b, workflow())
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two runs differ")
	}
	dec := xml.NewDecoder(bytes.NewReader(a.Bytes()))
	var root string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("not well-formed XML: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok && root == "" {
			root = se.Name.Local
		}
	}
	if root != "plist" {
		t.Fatalf("root element = %q", root)
	}
	if !strings.Contains(a.String(), string(rune(0xFFFC))) {
		t.Fatal("variable placeholder missing from the URL field")
	}
}
