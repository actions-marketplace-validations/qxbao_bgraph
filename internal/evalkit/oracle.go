package evalkit

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// MapEntry records how a FAIL_TO_PASS id was mapped to a test file (for oracle auditing).
type MapEntry struct {
	ID        string   `json:"id"`
	Files     []string `json:"files,omitempty"`
	Method    string   `json:"method"` // path | title | path_norm | suffix | none
	Title     string   `json:"title,omitempty"`
	Ambiguous bool     `json:"ambiguous,omitempty"` // title matched several files
}

const minTitleLen = 6

var idPathRe = regexp.MustCompile(`[A-Za-z0-9_@./\-]+\.(?:[cm]?[jt]sx?|vue|svelte)\b`)

// Mapping methods:
//
//	path       id contains the file path
//	path_p     id contains a file path that exists at P but was not changed by test_patch (extension; labelled)
//	title      file content contains the last title fragment of the id (split on " > ", " › ", "::"; whitespace collapsed; dropped if < 6 chars)
//	path_norm  id (alphanumeric-normalised) lies within the (normalised) path of a JS/TS source file: Babel's directory-style fixtures
//	suffix     fallback: word suffix of the title (≥ 3 words, ≥ 12 chars) found in the content (mocha joins descriptions with spaces)
//
// Order tried: path, path_p, title, path_norm, suffix; stops at the first method that yields a result. Three extension methods (path_p, path_norm, suffix) compared
// to the original spec are labelled for auditing.
func MapF2P(ids []string, files map[string]string, existsInP func(string) bool) (required []string, mapping []MapEntry, unmapped []string) {
	names := make([]string, 0, len(files))
	for p := range files {
		if candidateFile(p) {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	norm := map[string]string{}
	for _, p := range names {
		norm[p] = strings.Join(strings.Fields(files[p]), " ")
	}
	req := map[string]bool{}
	for _, id := range ids {
		e := MapEntry{ID: id, Method: "none"}
		for _, p := range names {
			if strings.Contains(id, p) {
				e.Files = append(e.Files, p)
			}
		}
		if len(e.Files) > 0 {
			e.Method = "path"
		}
		if e.Method == "none" && existsInP != nil {
			// The path is in the id but not changed by test_patch (shared runner, e.g. tests/runtime/test.ts): still a required test file.
			for _, m := range idPathRe.FindAllString(id, -1) {
				if strings.Contains(m, "/") && existsInP(m) {
					e.Files = append(e.Files, m)
				}
			}
			if len(e.Files) > 0 {
				e.Method = "path_p"
			}
		}
		title := lastSegment(id)
		e.Title = title
		if e.Method == "none" && len([]rune(title)) >= minTitleLen {
			for _, p := range names {
				if strings.Contains(norm[p], title) {
					e.Files = append(e.Files, p)
				}
			}
			if len(e.Files) > 0 {
				e.Method = "title"
				e.Ambiguous = len(e.Files) > 1
			}
		}
		if e.Method == "none" {
			nid := alnum(id)
			if len(nid) >= 10 {
				for _, p := range names {
					if isCodeExt(p) && strings.Contains(alnum(p), nid) {
						e.Files = append(e.Files, p)
					}
				}
				if len(e.Files) > 0 {
					e.Method = "path_norm"
					e.Ambiguous = len(e.Files) > 1
				}
			}
		}
		if e.Method == "none" {
			w := strings.Fields(title)
			for i := 1; i+3 <= len(w) && len(e.Files) == 0; i++ {
				suf := strings.Join(w[i:], " ")
				if len(suf) < 12 {
					break
				}
				for _, p := range names {
					if strings.Contains(norm[p], suf) {
						e.Files = append(e.Files, p)
					}
				}
			}
			if len(e.Files) > 0 {
				e.Method = "suffix"
				e.Ambiguous = len(e.Files) > 1
			}
		}
		if e.Method == "none" {
			unmapped = append(unmapped, id)
		}
		for _, f := range e.Files {
			req[f] = true
		}
		mapping = append(mapping, e)
	}
	for f := range req {
		required = append(required, f)
	}
	sort.Strings(required)
	return
}

func lastSegment(id string) string {
	s := id
	for _, sep := range []string{" > ", " › ", "::"} {
		if i := strings.LastIndex(s, sep); i >= 0 {
			s = s[i+len(sep):]
		}
	}
	return strings.Join(strings.Fields(s), " ")
}

func alnum(s string) string {
	var b strings.Builder
	sp := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			sp = false
		} else if !sp {
			b.WriteByte(' ')
			sp = true
		}
	}
	return strings.TrimSpace(b.String())
}

func isCodeExt(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts", ".vue", ".svelte":
		return true
	}
	return false
}

// candidateFile: a file that may contain tests (excludes snapshots, docs, images...).
func candidateFile(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".snap", ".md", ".mdx", ".txt", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".lock", ".map", ".woff", ".woff2", ".ttf", ".zip", ".gz":
		return false
	}
	return true
}
