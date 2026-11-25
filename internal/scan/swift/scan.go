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
		case processAPI(ts, i):
			process = true
		case i+2 < len(ts) && fileAPI(ts, i):
			arg := i + 2
			switch {
			case literalArgument(ts[arg:]):
				literals[arg] = true
			default:
				fileUnknown = fileUnknown || (!literalFileURL(ts[arg:]) && !bundleResource(ts[arg:]))
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
		case "at":
			if i < 2 || !tokenIs(ts[i-1], Punctuation, "(") {
				return false
			}
			switch ts[i-2].Text {
			case "contentsOfDirectory", "enumerator", "removeItem", "createDirectory", "trashItem":
				return true
			}
		case "fileURLWithPath", "filePath", "contentsOfFile", "contentsAtPath", "atPath",
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
	if n >= 1 && prefix[n].Text == "init" && prefix[n-1].Text == "." {
		// A contextually typed .init(contentsOf:) can be a Foundation file read.
		return true
	}
	switch prefix[n].Text {
	case "Data", "NSData", "String", "NSString", "NSDictionary", "NSArray", "NSAttributedString":
		return true
	}
	return false
}

func literalArgument(ts []Token) bool {
	return len(ts) > 1 && ts[0].Kind == String && operandEnd(ts[1:])
}

func literalFileURL(ts []Token) bool {
	return len(ts) > 5 && tokenIs(ts[0], Word, "URL") && tokenIs(ts[1], Punctuation, "(") &&
		tokenIs(ts[2], Word, "fileURLWithPath") && tokenIs(ts[3], Punctuation, ":") &&
		ts[4].Kind == String && tokenIs(ts[5], Punctuation, ")") && operandEnd(ts[6:])
}

func bundleResource(ts []Token) bool {
	// A direct lookup for a literal basename stays inside the resource bundle.
	// A URL derived from that lookup may walk outside it.
	if len(ts) < 15 || !matchesTokens(ts, "Bundle", ".", "module", ".", "url", "(", "forResource", ":") ||
		!simpleResourceName(ts[8]) || !matchesTokens(ts[9:], ",", "withExtension", ":") ||
		(!simpleResourceName(ts[12]) && !tokenIs(ts[12], Word, "nil")) || !tokenIs(ts[13], Punctuation, ")") {
		return false
	}
	end := ts[14:]
	if len(end) > 0 && tokenIs(end[0], Punctuation, "!") {
		end = end[1:]
	}
	return operandEnd(end)
}

func matchesTokens(ts []Token, parts ...string) bool {
	if len(ts) < len(parts) {
		return false
	}
	for i, part := range parts {
		if ts[i].Kind > Punctuation || ts[i].Text != part {
			return false
		}
	}
	return true
}

func simpleResourceName(t Token) bool {
	return t.Kind == String && t.Text != "." && t.Text != ".." && !strings.ContainsAny(t.Text, "/\\")
}

func operandEnd(ts []Token) bool {
	return len(ts) > 0 && (tokenIs(ts[0], Punctuation, ",") || tokenIs(ts[0], Punctuation, ")"))
}

func processAPI(ts []Token, i int) bool {
	switch ts[i].Text {
	case "Process", "Subprocess", "NSTask", "launchPath", "executableURL":
		return true
	case "posix_spawn", "posix_spawnp", "execve", "execvp", "execv", "system", "popen":
		// C functions count only as a direct call. A member such as
		// `.system(size:)` in SwiftUI shares the name but launches nothing.
		return i+1 < len(ts) && tokenIs(ts[i+1], Punctuation, "(") &&
			(i == 0 || !tokenIs(ts[i-1], Punctuation, "."))
	}
	return false
}

func tokenIs(t Token, kind Kind, text string) bool {
	return t.Kind == kind && t.Text == text
}
