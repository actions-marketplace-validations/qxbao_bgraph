package cpp

import (
	"bytes"
	"path"
	"regexp"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

// Native reports whether a file can contain a C-family preprocessor directive.
func Native(f string) bool {
	switch strings.ToLower(path.Ext(f)) {
	case ".c", ".cc", ".cpp", ".cxx", ".c++", ".cp", ".m", ".mm",
		".h", ".hh", ".hpp", ".hxx", ".h++", ".inc", ".inl", ".ipp", ".tpp",
		".ixx", ".cppm", ".s", ".cu", ".cuh":
		return true
	}
	return false
}

// Source records literal preprocessor include and embed directives. Other
// spellings that could hide an input force conservative ownership.
func Source(src []byte) model.ScanResult {
	s := strings.TrimPrefix(string(src), "\ufeff")
	if strings.Contains(s, "??") || strings.Contains(s, "\\\r") {
		return uncertain("C/C++ alternative spelling or line continuation")
	}
	s = strings.ReplaceAll(s, "\\\n", "")
	var out model.ScanResult
	line := 1
	start := true
	for len(s) > 0 {
		switch {
		case strings.HasPrefix(s, "//"):
			if end := strings.IndexByte(s, '\n'); end != -1 {
				s = s[end:]
				continue
			}
			return out
		case strings.HasPrefix(s, "/*"):
			end := strings.Index(s[2:], "*/")
			if end == -1 {
				return uncertain("unterminated C/C++ block comment")
			}
			n := strings.Count(s[:end+4], "\n")
			if n > 0 {
				start = true
				line += n
			}
			s = s[end+4:]
		case s[0] == '\n':
			line++
			start = true
			s = s[1:]
		case strings.ContainsRune(" \t\v\f\r", rune(s[0])):
			s = s[1:]
		case start && (s[0] == '#' || strings.HasPrefix(s, "%:")):
			n := 1
			if s[0] == '%' {
				n = 2
			}
			tail, ok := directive(s[n:])
			if !ok {
				return uncertain("C/C++ directive with an uncertain comment")
			}
			match := directiveName.FindStringSubmatch(tail)
			if len(match) != 0 {
				if strings.Contains(match[2], "##") || strings.Contains(match[2], "%:%:") {
					return uncertain("C/C++ token-pasting macro")
				}
				switch match[1] {
				case "include", "include_next", "import", "embed":
					m := includeName.FindStringSubmatch(match[2])
					if len(m) == 0 || strings.ContainsAny(m[1]+m[2], "\\\x00") {
						return uncertain("nonliteral C/C++ include or embed")
					}
					kind := model.ImportStatic
					if m[2] != "" {
						kind = model.ImportSystem
					}
					out.Imports = append(out.Imports, model.ImportRef{Kind: kind, Spec: m[1] + m[2], Suffix: match[1], Line: line})
				case "pragma":
					if strings.Contains(match[2], "comment") || strings.Contains(match[2], "dependency") {
						return uncertain("C/C++ pragma linker input")
					}
				}
			}
			// Continue lexing the directive to find implicit input probes.
			// Include paths themselves may contain arbitrary punctuation.
			switch {
			case len(match) == 0, match[1] == "include", match[1] == "include_next",
				match[1] == "import", match[1] == "embed":
				end := strings.IndexByte(s, '\n')
				if end == -1 {
					return out
				}
				s = s[end:]
			default:
				s = s[n:]
				start = false
			}
		case rawPrefix(s) != 0:
			prefix := rawPrefix(s)
			open := strings.IndexByte(s, '(')
			if open < prefix || open > prefix+16 {
				return uncertain("unsupported C++ raw string delimiter")
			}
			end := ")" + s[prefix:open] + "\""
			i := strings.Index(s[open+1:], end)
			if i < 0 {
				return uncertain("unterminated C++ raw string")
			}
			endAt := open + 1 + i + len(end)
			line += strings.Count(s[:endAt], "\n")
			s = s[endAt:]
			start = false
		case s[0] == '"' || s[0] == '\'':
			i := quotedEnd(s)
			if i < 0 {
				return uncertain("unterminated C/C++ quoted token")
			}
			s = s[i:]
			start = false
		case identifierByte(s[0]):
			i := 1
			for i < len(s) {
				if identifierByte(s[i]) {
					i++
				} else if isDigit(s[0]) && s[i] == '\'' && i+1 < len(s) && identifierByte(s[i+1]) {
					// A C++14 digit separator continues a number token.
					i += 2
				} else {
					break
				}
			}
			word := s[:i]
			switch {
			case word == "import" || word == "module":
				return uncertain("possible C++ module dependency")
			case runtimeAPI[word]:
				return uncertain("C/C++ runtime or implicit input via " + word)
			}
			s = s[i:]
			start = false
		default:
			s = s[1:]
			start = false
		}
	}
	return out
}

var directiveName = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)(.*)$`)
var headerDirective = regexp.MustCompile(`^[ \t]*(?:include|include_next|import|embed)[ \t]*$`)
var includeName = regexp.MustCompile(`^[ \t]*(?:"([^"]+)"|<([^>]+)>)(?:[ \t].*)?$`)

// These entry points can name paths or executables at runtime, including
// files otherwise owned by unrelated targets or projects.
var runtimeAPI = map[string]bool{
	"open": true, "openat": true, "fopen": true, "fopen_s": true, "freopen": true, "fdopen": true,
	"_open": true, "_wopen": true, "_wfopen": true, "_wfopen_s": true,
	"filesystem": true, "ifstream": true, "wifstream": true, "fstream": true, "wfstream": true,
	"opendir": true, "readdir": true, "scandir": true, "glob": true, "stat": true, "lstat": true, "access": true,
	"system": true, "_wsystem": true, "popen": true, "_popen": true, "_wpopen": true,
	"execv": true, "execve": true, "execvp": true, "execvpe": true, "execl": true, "execle": true, "execlp": true,
	"posix_spawn": true, "posix_spawnp": true, "fork": true, "vfork": true,
	"dlopen": true, "dlsym": true, "LoadLibrary": true, "LoadLibraryA": true, "LoadLibraryW": true,
	"CreateFile": true, "CreateFileA": true, "CreateFileW": true, "ReadFile": true,
	"CreateProcess": true, "CreateProcessA": true, "CreateProcessW": true, "ShellExecute": true,
	"asm": true, "__asm": true, "__asm__": true, "__has_include": true, "__has_include_next": true, "__has_embed": true,
}

func identifierByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// inNumber reports whether text ends inside a preprocessing number, where a
// quote is a C++14 digit separator rather than a character literal.
func inNumber(text string) bool {
	i := len(text)
	for i > 0 && (identifierByte(text[i-1]) || text[i-1] == '\'' || text[i-1] == '.') {
		i--
	}
	for i < len(text) && text[i] == '.' {
		i++
	}
	return i < len(text) && isDigit(text[i]) && identifierByte(text[len(text)-1])
}

func rawPrefix(s string) int {
	for _, p := range []string{`R"`, `u8R"`, `uR"`, `UR"`, `LR"`} {
		if strings.HasPrefix(s, p) {
			return len(p)
		}
	}
	return 0
}

// directive strips leading block comments after the introducer and between
// the directive's name and argument. A multiline comment can change which
// logical line contains the tokens, so reject that spelling conservatively.
func directive(s string) (string, bool) {
	end := strings.IndexByte(s, '\n')
	if end != -1 {
		s = s[:end]
	}
	s = strings.TrimSpace(s)
	var b bytes.Buffer
	for len(s) > 0 {
		switch {
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s[2:], "*/")
			if i < 0 {
				return "", false
			}
			b.WriteByte(' ')
			s = s[i+4:]
		case strings.HasPrefix(s, "//"):
			return strings.TrimSpace(b.String()), true
		case s[0] == '<' && headerDirective.MatchString(b.String()):
			i := strings.IndexByte(s[1:], '>')
			if i < 0 {
				return "", false
			}
			b.WriteString(s[:i+2])
			s = s[i+2:]
		case s[0] == '\'' && inNumber(b.String()):
			b.WriteByte(s[0])
			s = s[1:]
		case s[0] == '"' || s[0] == '\'':
			i := quotedEnd(s)
			if i < 0 {
				return "", false
			}
			b.WriteString(s[:i])
			s = s[i:]
		default:
			b.WriteByte(s[0])
			s = s[1:]
		}
	}
	return strings.TrimSpace(b.String()), true
}

func quotedEnd(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case s[0]:
			return i + 1
		case '\\':
			i++
		case '\n':
			return -1
		}
	}
	return -1
}

func uncertain(detail string) model.ScanResult {
	return model.ScanResult{Uncertain: true, UncertainReason: detail}
}
