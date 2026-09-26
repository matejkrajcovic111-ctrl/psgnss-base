package web

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// jsString decodes a single- or double-quoted JavaScript string literal, for
// the escapes this UI actually uses.
func jsString(lit string) string {
	body := lit[1 : len(lit)-1]
	r := strings.NewReplacer(`\n`, "\n", `\'`, "'", `\"`, `"`, `\\`, `\`)
	return r.Replace(body)
}

// Every string the page passes to t() as a literal has a Slovak entry. A
// missing one would not break anything -- t() falls back to the English -- so
// without this a new label would quietly stay English in the Slovak interface.
func TestEveryTranslatedLiteralHasASlovakEntry(t *testing.T) {
	app := readAsset(t, "assets/app.js")
	dict := readAsset(t, "assets/i18n.js")

	have := map[string]bool{}
	entry := regexp.MustCompile(`(?m)^  ("(?:[^"\\]|\\.)*"): `)
	for _, m := range entry.FindAllStringSubmatch(dict, -1) {
		k, err := strconv.Unquote(m[1])
		if err != nil {
			t.Fatalf("dictionary key %s does not parse: %v", m[1], err)
		}
		have[k] = true
	}
	if len(have) < 600 {
		t.Fatalf("found only %d Slovak entries; the dictionary format changed", len(have))
	}

	// t('...'), t("..."), and both branches of t(cond ? '...' : '...').
	lit := `('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*")`
	call := regexp.MustCompile(`\bt\(\s*` + lit)
	branch := regexp.MustCompile(`\bt\([^()'"]*\?\s*` + lit + `\s*:\s*` + lit)
	checked := 0
	for _, re := range []*regexp.Regexp{call, branch} {
		for _, m := range re.FindAllStringSubmatch(app, -1) {
			for _, l := range m[1:] {
				k := jsString(l)
				checked++
				if !have[k] {
					t.Errorf("no Slovak entry for %q", k)
				}
			}
		}
	}
	if checked < 400 {
		t.Fatalf("checked only %d t() literals; the pattern no longer matches the page", checked)
	}
}

// The page loads the dictionary before the application that calls it.
func TestDictionaryLoadsBeforeTheApplication(t *testing.T) {
	index := readAsset(t, "assets/index.html")
	i, a := strings.Index(index, `src="i18n.js"`), strings.Index(index, `src="app.js"`)
	if i < 0 || a < 0 || i > a {
		t.Fatal("index.html must load i18n.js before app.js")
	}
}
