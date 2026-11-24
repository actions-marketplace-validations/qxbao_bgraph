package swift

import (
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

// Scan reports imports, possible runtime paths and process launches. Manifest
// declarations provide most build dependencies. Imports also catch a module
// SwiftPM made available to a target through a shared test build or build cache.
func Scan(src []byte) model.ScanResult {
	ts, err := Lex(src)
	var out model.ScanResult
	if err != nil {
		out.Uncertain = true
		out.UncertainReason = err.Error()
	}
	process := false
	literals := map[int]bool{}
	fileUnknown := false
	for i, t := range ts {
		if t.Kind != Word {
			continue
		}
		switch {
		case t.Text == "import":
			if name := importedModule(ts[i+1:]); name != "" {
				out.Imports = append(out.Imports, model.ImportRef{Kind: model.ImportStatic, Spec: name})
			}
		case t.Text == "canImport" && i+1 < len(ts) && tokenIs(ts[i+1], Punctuation, "("):
			if name := importedModule(ts[i+2:]); name != "" {
				out.Imports = append(out.Imports, model.ImportRef{Kind: model.ImportStatic, Spec: name})
			}
		case processAPI(t.Text):
			process = true
		case i+2 < len(ts) && fileAPI(ts, i):
			arg := i + 2
			switch {
			case literalArgument(ts[arg:]):
				literals[arg] = true
			default:
				if !literalFileURL(ts[arg:]) && !bundleResource(ts[arg:]) {
					fileUnknown = true
				}
			}
		}
	}
	for i, t := range ts {
		if t.Kind != String && t.Kind != ComputedString {
			continue
		}
		s := strings.TrimSpace(t.Text)
		if s == "" || strings.ContainsAny(s, "\r\n\t\x00") || (strings.Contains(s, "://") && !strings.HasPrefix(s, "file://")) {
			continue
		}
		if !strings.ContainsAny(s, "/\\.") && !process && !literals[i] {
			continue
		}
		if !literals[i] && (s == "." || s == ".." || (process && (s == "/" || path.Clean(s) == "."))) {
			continue
		}
		ref := model.ImportRef{Kind: model.ImportRead, Spec: s, Dynamic: t.Kind == ComputedString}
		switch {
		case literals[i]:
			ref.Suffix = "file"
		case process:
			ref.Suffix = "process"
		}
		out.Imports = append(out.Imports, ref)
	}
	if process {
		// An executable can be constructed in a helper or read from the environment.
		out.Imports = append(out.Imports, model.ImportRef{Kind: model.ImportRead, Dynamic: true, Suffix: "process"})
	}
	if fileUnknown {
		out.Imports = append(out.Imports, model.ImportRef{Kind: model.ImportRead, Dynamic: true})
	}
	return out
}

func importedModule(ts []Token) string {
	if len(ts) == 0 {
		return ""
	}
	switch ts[0].Text {
	case "typealias", "struct", "class", "enum", "protocol", "let", "var", "func":
		ts = ts[1:]
	}
	if len(ts) > 0 && ts[0].Text == "\x60" {
		ts = ts[1:]
	}
	if len(ts) > 0 && ts[0].Kind == Word {
		return ts[0].Text
	}
	return ""
}

func fileAPI(ts []Token, i int) bool {
	name, next := ts[i].Text, ts[i+1]
	if next.Kind != Punctuation {
		return false
	}
	switch next.Text {
	case ":":
		switch name {
		case "contentsOf":
			return fileContentsInitializer(ts[:i])
		case "fileURLWithPath", "contentsOfFile", "contentsAtPath", "atPath",
			"ofItemAtPath", "destinationOfSymbolicLinkAtPath", "forReadingFrom", "forWritingTo", "fromPath", "toPath":
			return true
		}
	case "(":
		switch name {
		case "open", "fopen", "opendir", "stat", "lstat", "readlink", "realpath", "appendingPathComponent":
			return true
		}
	}
	return false
}

func fileContentsInitializer(prefix []Token) bool {
	if len(prefix) < 2 || prefix[len(prefix)-1].Text != "(" {
		return false
	}
	n := len(prefix) - 2
	if n >= 2 && prefix[n].Text == "init" && prefix[n-1].Text == "." {
		n -= 2
	}
	switch prefix[n].Text {
	case "Data", "NSData", "String", "NSString", "NSDictionary", "NSArray", "NSAttributedString":
		return true
	}
	return false
}

func literalArgument(ts []Token) bool {
	return len(ts) > 1 && ts[0].Kind == String && (ts[1].Text == "," || ts[1].Text == ")")
}

func literalFileURL(ts []Token) bool {
	return len(ts) > 4 && tokenIs(ts[0], Word, "URL") && tokenIs(ts[1], Punctuation, "(") &&
		tokenIs(ts[2], Word, "fileURLWithPath") && tokenIs(ts[3], Punctuation, ":") && literalArgument(ts[4:])
}

func bundleResource(ts []Token) bool {
	return len(ts) > 2 && tokenIs(ts[0], Word, "Bundle") && tokenIs(ts[1], Punctuation, ".") &&
		tokenIs(ts[2], Word, "module")
}

func processAPI(word string) bool {
	switch word {
	case "Process", "Subprocess", "NSTask", "posix_spawn", "posix_spawnp", "execve", "execvp", "execv", "system", "popen", "launchPath", "executableURL":
		return true
	}
	return false
}

func tokenIs(t Token, kind Kind, text string) bool {
	return t.Kind == kind && t.Text == text
}
