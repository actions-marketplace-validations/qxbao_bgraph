package jvm

import (
	"path"
	"strings"
)

// settingsInfo is what the settings script of a Gradle build declares, read as text.
type settingsInfo struct {
	includes      []string          // include(...) arguments as written
	flat          []string          // includeFlat(...) arguments
	includeBuilds []string          // includeBuild(...) arguments
	dirs          map[string]string // normalized project path → projectDir relative to the settings directory
	applied       []string          // literal `apply from:` targets, relative to the settings directory
	rootName      string
	// dynamic lists the constructs that make the project list or layout unknowable lexically.
	dynamic []string
}

func normProjectPath(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, ":") {
		s = ":" + s
	}
	s = strings.TrimRight(s, ":")
	if s == "" {
		return ":"
	}
	return s
}

// rootDirPrefixes are the leading forms that denote the settings/root directory inside a path string.
var rootDirPrefixes = []string{"${rootDir}/", "$rootDir/", "${settingsDir}/", "$settingsDir/", "${rootProject.projectDir}/",
	"$rootProject.projectDir/", "${project.rootDir}/", "$project.rootDir/", "${rootProject.rootDir}/", "$rootProject.rootDir/",
	"${project.rootProject.projectDir}/", "${settings.rootDir}/", "${settings.settingsDir}/"}

// stripRootDir removes a leading root-directory reference. ok=false when the string still has an interpolation or is absolute.
func stripRootDir(s string) (string, bool) {
	for _, p := range rootDirPrefixes {
		if r, found := strings.CutPrefix(s, p); found {
			s = r
			break
		}
	}
	if strings.Contains(s, "$") || strings.HasPrefix(s, "/") || (len(s) > 1 && s[1] == ':') {
		return "", false
	}
	return strings.ReplaceAll(s, "\\", "/"), true
}

func isPunct(t token, s string) bool { return t.kind == tPunct && t.text == s }
func isIdent(t token, s string) bool { return t.kind == tIdent && t.text == s }

// callStrings reads the argument list of a call whose name is toks[i]: `name("a", "b")`, `name "a", "b"`. It returns the literal
// strings, the index after the arguments and ok=false when any argument is not a plain string literal.
func callStrings(toks []token, i int) (vals []string, next int, ok bool) {
	j := i + 1
	paren := j < len(toks) && isPunct(toks[j], "(")
	if paren {
		j++
	}
	for j < len(toks) {
		if toks[j].kind != tStr || toks[j].dyn {
			return vals, j, false
		}
		vals = append(vals, toks[j].text)
		j++
		if j < len(toks) && isPunct(toks[j], ",") {
			j++
			if paren && j < len(toks) && isPunct(toks[j], ")") {
				break // trailing comma (Kotlin)
			}
			continue
		}
		break
	}
	if len(vals) == 0 {
		return nil, j, false
	}
	if paren {
		// `include("a") { ... }` is not valid, but includeBuild("x") { ... } is: only the closing parenthesis matters.
		if j >= len(toks) || !isPunct(toks[j], ")") {
			return vals, j, false
		}
		j++
	}
	return vals, j, true
}

// parseDirExpr parses the right-hand side of a projectDir assignment starting at toks[j].
func parseDirExpr(toks []token, j int) (string, bool) {
	if j < len(toks) && isIdent(toks[j], "new") {
		j++
	}
	if j+1 >= len(toks) || toks[j].kind != tIdent || !isPunct(toks[j+1], "(") {
		return "", false
	}
	if toks[j].text != "file" && toks[j].text != "File" {
		return "", false
	}
	j += 2
	if j >= len(toks) {
		return "", false
	}
	// File(settingsDir, "x") / File(rootDir, "x")
	if toks[j].kind == tIdent {
		k := j
		hasRoot := false
		for k < len(toks) && !isPunct(toks[k], ",") && !isPunct(toks[k], ")") {
			if isIdent(toks[k], "rootDir") || isIdent(toks[k], "settingsDir") {
				hasRoot = true
			}
			k++
		}
		if !hasRoot || k+2 >= len(toks) || !isPunct(toks[k], ",") || toks[k+1].kind != tStr || toks[k+1].dyn {
			return "", false
		}
		return cleanRel(toks[k+1].text), true
	}
	if toks[j].kind != tStr {
		return "", false
	}
	s := toks[j].text
	if toks[j].dyn {
		r, ok := stripRootDir(s)
		if !ok {
			return "", false
		}
		return cleanRel(r), true
	}
	return cleanRel(s), true
}

func cleanRel(s string) string {
	return path.Clean(strings.ReplaceAll(s, "\\", "/"))
}

// parseSettings reads a settings script.
func parseSettings(src string, kotlin bool) settingsInfo {
	si := settingsInfo{dirs: map[string]string{}}
	toks, ok := lexGradle(src, kotlin)
	if !ok {
		si.dynamic = append(si.dynamic, "settings script cannot be tokenized")
		return si
	}
	consumedDirs := 0
	totalDirs := 0
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tIdent {
			continue
		}
		prevDot := i > 0 && isPunct(toks[i-1], ".")
		viaSettings := prevDot && i > 1 && isIdent(toks[i-2], "settings")
		switch t.text {
		case "include", "includeFlat", "includeBuild":
			if prevDot && !viaSettings {
				continue
			}
			vals, next, ok := callStrings(toks, i)
			if !ok {
				si.dynamic = append(si.dynamic, t.text+" with a non-literal argument (line "+itoa(t.line)+")")
				if next > i {
					i = next - 1
				}
				continue
			}
			switch t.text {
			case "include":
				si.includes = append(si.includes, vals...)
			case "includeFlat":
				si.flat = append(si.flat, vals...)
			default:
				si.includeBuilds = append(si.includeBuilds, vals...)
			}
			i = next - 1
		case "setProjectDir", "setName":
			si.dynamic = append(si.dynamic, t.text+" on a project descriptor (line "+itoa(t.line)+")")
		case "name":
			// project(":x").name = "y" / it.name = ...: the project path changes. rootProject.name only names the build, and a bare
			// `name = ` inside a block is the name of a repository or cache.
			if prevDot && i+1 < len(toks) && isPunct(toks[i+1], "=") && !(i+2 < len(toks) && isPunct(toks[i+2], "=")) &&
				!(i > 1 && isIdent(toks[i-2], "rootProject")) {
				si.dynamic = append(si.dynamic, "a project is renamed (line "+itoa(t.line)+")")
			}
		case "projectDir":
			if i+1 < len(toks) && isPunct(toks[i+1], "=") && !(i+2 < len(toks) && isPunct(toks[i+2], "=")) {
				// Without a receiver (inside apply { } / with(..) { }) the assignment cannot be attributed: counted, not consumed.
				totalDirs++
				if !prevDot {
					continue
				}
				// project(":x").projectDir = ... / findProject(":x")?.projectDir = ...
				k := i - 2 // token before '.'
				if k >= 0 && isPunct(toks[k], "?") {
					k--
				}
				if k >= 3 && isPunct(toks[k], ")") && toks[k-1].kind == tStr && !toks[k-1].dyn && isPunct(toks[k-2], "(") &&
					(isIdent(toks[k-3], "project") || isIdent(toks[k-3], "findProject")) {
					if d, ok := parseDirExpr(toks, i+2); ok {
						si.dirs[normProjectPath(toks[k-1].text)] = d
						consumedDirs++
					}
				}
			}
		case "buildFileName":
			si.dynamic = append(si.dynamic, "buildFileName is customized (line "+itoa(t.line)+")")
		case "rootProject":
			if i+4 < len(toks) && isPunct(toks[i+1], ".") && isIdent(toks[i+2], "name") && isPunct(toks[i+3], "=") && toks[i+4].kind == tStr {
				si.rootName = toks[i+4].text
			}
		case "apply":
			// apply from: "x.gradle" / apply(from = "x.gradle")
			for j := i + 1; j < len(toks) && j < i+4; j++ {
				if isIdent(toks[j], "from") {
					found := false
					for k := j + 1; k < len(toks) && k < j+10; k++ {
						if toks[k].kind == tStr {
							if r, ok := stripRootDir(toks[k].text); ok && !strings.Contains(r, "://") {
								si.applied = append(si.applied, cleanRel(r))
								found = true
							}
							break
						}
					}
					if !found {
						si.dynamic = append(si.dynamic, "apply from a non-literal script (line "+itoa(t.line)+")")
					}
					break
				}
			}
		}
	}
	if totalDirs != consumedDirs {
		si.dynamic = append(si.dynamic, "projectDir assigned from a non-literal expression")
	}
	return si
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
