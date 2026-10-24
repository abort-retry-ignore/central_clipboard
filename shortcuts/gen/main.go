// Command gen writes the unsigned "Clipboard" iPhone shortcut to stdout: one
// shortcut with a Send / Get menu that talks to the hub. iOS only imports
// signed shortcuts, and signing needs a Mac: run scripts/sign-shortcut.sh.
//
// The shortcut holds no hub address and no token. It asks for the URL
// (http://<hub>:8766/?t=<token>, from the hub's /join page) when it is
// imported, so one signed file works for every hub.
package main

import (
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type dict = map[string]any

// uuid is derived from a name so the output is the same on every run.
func uuid(name string) string {
	h := sha256.Sum256([]byte("central-clipboard/" + name))
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16]))
}

func act(ident string, params dict) dict {
	return dict{"WFWorkflowActionIdentifier": "is.workflow.actions." + ident, "WFWorkflowActionParameters": params}
}

func outputRef(name, id string) dict {
	return dict{"OutputName": name, "OutputUUID": id, "Type": "ActionOutput"}
}

// attachment makes a whole field equal to a previous action's output.
func attachment(name, id string) dict {
	return dict{"Value": outputRef(name, id), "WFSerializationType": "WFTextTokenAttachment"}
}

// textWith embeds a previous action's output in a text field (the URL field needs this form).
func textWith(name, id string) dict {
	return dict{
		"Value": dict{
			"string":             string(rune(0xFFFC)), // the object replacement character marks the variable
			"attachmentsByRange": dict{"{0, 1}": outputRef(name, id)},
		},
		"WFSerializationType": "WFTextTokenString",
	}
}

const importQuestion = "Paste your hub URL from the hub's /join page. It looks like http://100.x.x.x:8766/?t=…"

func workflow() dict {
	url, clip, post, get := uuid("url"), uuid("clipboard"), uuid("post"), uuid("get")
	group := uuid("menu")
	menu := func(mode int, params dict) dict {
		p := dict{"GroupingIdentifier": group, "WFControlFlowMode": mode}
		for k, v := range params {
			p[k] = v
		}
		return act("choosefrommenu", p)
	}
	return dict{
		"WFWorkflowActions": []any{
			act("gettext", dict{"UUID": url, "WFTextActionText": ""}),
			menu(0, dict{"WFMenuPrompt": "Clipboard", "WFMenuItems": []any{"Send", "Get"}}),
			menu(1, dict{"WFMenuItemTitle": "Send"}),
			act("getclipboard", dict{"UUID": clip}),
			act("downloadurl", dict{
				"UUID": post, "WFURL": textWith("Text", url), "WFHTTPMethod": "POST",
				"WFHTTPBodyType": "File", "WFRequestVariable": attachment("Clipboard", clip)}),
			menu(1, dict{"WFMenuItemTitle": "Get"}),
			act("downloadurl", dict{"UUID": get, "WFURL": textWith("Text", url), "WFHTTPMethod": "GET"}),
			act("setclipboard", dict{"WFInput": attachment("Contents of URL", get)}),
			menu(2, nil),
		},
		"WFWorkflowClientVersion":        "2605.0.5",
		"WFWorkflowMinimumClientVersion": 900,
		"WFWorkflowIcon":                 dict{"WFWorkflowIconStartColor": 4282601983, "WFWorkflowIconGlyphNumber": 59511},
		"WFWorkflowImportQuestions": []any{dict{
			"ActionIndex": 0, "Category": "Parameter", "ParameterKey": "WFTextActionText",
			"DefaultValue": "", "Text": importQuestion}},
		"WFWorkflowInputContentItemClasses": []any{},
		"WFWorkflowTypes":                   []any{},
	}
}

// writePlist writes v as an XML property list; map keys are sorted so output is stable.
func writePlist(w io.Writer, v any) error {
	fmt.Fprint(w, xml.Header, `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`+"\n", `<plist version="1.0">`+"\n")
	if err := writeValue(w, v, 0); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "</plist>")
	return err
}

func writeValue(w io.Writer, v any, depth int) error {
	pad := strings.Repeat("\t", depth)
	switch v := v.(type) {
	case string:
		fmt.Fprint(w, pad, "<string>")
		if err := xml.EscapeText(w, []byte(v)); err != nil {
			return err
		}
		fmt.Fprintln(w, "</string>")
	case int:
		fmt.Fprintf(w, "%s<integer>%d</integer>\n", pad, v)
	case bool:
		fmt.Fprintf(w, "%s<%t/>\n", pad, v)
	case []any:
		fmt.Fprintf(w, "%s<array>\n", pad)
		for _, e := range v {
			if err := writeValue(w, e, depth+1); err != nil {
				return err
			}
		}
		fmt.Fprintf(w, "%s</array>\n", pad)
	case dict:
		fmt.Fprintf(w, "%s<dict>\n", pad)
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprint(w, pad, "\t<key>")
			xml.EscapeText(w, []byte(k))
			fmt.Fprintln(w, "</key>")
			if err := writeValue(w, v[k], depth+1); err != nil {
				return err
			}
		}
		fmt.Fprintf(w, "%s</dict>\n", pad)
	default:
		return fmt.Errorf("unsupported plist value %T", v)
	}
	return nil
}

func main() {
	if err := writePlist(os.Stdout, workflow()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
