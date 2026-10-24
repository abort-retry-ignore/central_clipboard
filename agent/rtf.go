package main

import (
	"regexp"
	"strconv"
	"strings"
)

// rtfToText flattens RTF to plain text. iOS puts rich text on the clipboard,
// and most Linux apps cannot paste RTF. Adapted from the striprtf recipe.

var rtfToken = regexp.MustCompile(`(?is)\\([a-z]{1,32})(-?\d{1,10})?[ ]?|\\'([0-9a-f]{2})|\\([^a-z])|([{}])|[\r\n]+|(.)`)

var rtfDestinations = map[string]bool{
	"fonttbl": true, "colortbl": true, "expandedcolortbl": true, "stylesheet": true, "info": true,
	"pict": true, "object": true, "header": true, "footer": true, "footnote": true, "filetbl": true,
	"listtable": true, "listoverridetable": true, "listname": true, "listtext": true,
	"levelnumbers": true, "leveltext": true, "levelmarker": true, "themedata": true,
	"generator": true, "revtbl": true, "xmlnstbl": true, "datastore": true, "colorschememapping": true,
}

var rtfSpecial = map[string]string{
	"par": "\n", "sect": "\n\n", "page": "\n\n", "line": "\n", "tab": "\t",
	"emdash": "—", "endash": "–", "emspace": " ", "enspace": " ",
	"bullet": "•", "lquote": "‘", "rquote": "’",
	"ldblquote": "“", "rdblquote": "”",
}

// cp1252 maps bytes 0x80-0x9f; the rest of the range matches Latin-1.
var cp1252 = [32]rune{
	0x20ac, 0xfffd, 0x201a, 0x0192, 0x201e, 0x2026, 0x2020, 0x2021, 0x02c6, 0x2030, 0x0160, 0x2039, 0x0152, 0xfffd, 0x017d, 0xfffd,
	0xfffd, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014, 0x02dc, 0x2122, 0x0161, 0x203a, 0x0153, 0xfffd, 0x017e, 0x0178,
}

func cp1252Rune(b byte) rune {
	if b >= 0x80 && b < 0xa0 {
		return cp1252[b-0x80]
	}
	return rune(b)
}

func decodeCP1252(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		sb.WriteRune(cp1252Rune(c))
	}
	return sb.String()
}

func rtfToText(raw []byte) string {
	type frame struct {
		ucskip    int
		ignorable bool
	}
	var stack []frame
	var out strings.Builder
	ignorable, ucskip, curskip := false, 1, 0
	for _, m := range rtfToken.FindAllStringSubmatch(decodeCP1252(raw), -1) {
		word, arg, hexc, char, brace, tchar := m[1], m[2], m[3], m[4], m[5], m[6]
		switch {
		case brace == "{":
			curskip = 0
			stack = append(stack, frame{ucskip, ignorable})
		case brace == "}":
			curskip = 0
			if n := len(stack); n > 0 {
				ucskip, ignorable = stack[n-1].ucskip, stack[n-1].ignorable
				stack = stack[:n-1]
			}
		case char != "":
			curskip = 0
			switch {
			case char == "~":
				if !ignorable {
					out.WriteString(" ")
				}
			case char == "{" || char == "}" || char == "\\":
				if !ignorable {
					out.WriteString(char)
				}
			case char == "*":
				ignorable = true
			}
		case word != "":
			curskip = 0
			switch {
			case rtfDestinations[word]:
				ignorable = true
			case ignorable:
			case rtfSpecial[word] != "":
				out.WriteString(rtfSpecial[word])
			case word == "uc":
				ucskip, _ = strconv.Atoi(arg)
			case word == "u":
				c, _ := strconv.Atoi(arg)
				if c < 0 {
					c += 65536
				}
				out.WriteRune(rune(c))
				curskip = ucskip
			}
		case hexc != "":
			if curskip > 0 {
				curskip--
			} else if !ignorable {
				b, _ := strconv.ParseUint(hexc, 16, 8)
				out.WriteRune(cp1252Rune(byte(b)))
			}
		case tchar != "":
			if curskip > 0 {
				curskip--
			} else if !ignorable {
				out.WriteString(tchar)
			}
		}
	}
	return out.String()
}
