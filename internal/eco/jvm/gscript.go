package jvm

import (
	"path"
	"strings"
)

type refKind int

const (
	refPath        refKind = iota // project(":x") with a literal path
	refStrPath                    // a string literal ":x:y:task": the longest prefix naming a project
	refAccessor                   // projects.x.y
	refUnplaceable                // a reference whose target cannot be determined
	refFile                       // a path literal reaching outside the project directory
	refApply                      // apply from: "script.gradle"
	refWorkDir                    // a test working directory: val relative to the project, or to the build when rootRel
)

type scopeKind int

const (
	scopeSelf    scopeKind = iota // the project the script belongs to
	scopeAll                      // allprojects { }: the project and all below it
	scopeSub                      // subprojects { }: all below it
	scopeProject                  // project(":x") { }: that project
	scopeBuild                    // every project of the build (rootProject.allprojects { }, configure(...) { }, hooks)
)

type scopeInfo struct {
	kind scopeKind
	arg  string
}

type gref struct {
	kind    refKind
	val     string
	sc      scopeInfo
	rootRel bool // val is relative to the build (settings) directory instead of the project directory
	line    int
	// strict: a refPath that must name a project (project(..), evaluationDependsOn(..)): when it names none the project model is
	// wrong, so it depends on every project. findProject(..) may legitimately name an absent project.
	strict bool
	// mustExist: a refFile found by a heuristic (a string near rootProject/rootDir): an edge only when the path exists.
	mustExist bool
}

// projectSets are the Project properties that hold a collection of projects: navigating them (rootProject.subprojects.forEach
// { implementation(it) }) can reach any project.
var projectSets = map[string]bool{"allprojects": true, "subprojects": true, "childProjects": true}

// rootDirIdents name the root (settings) directory or project; a string literal following them in a call chain
// (rootProject.file("x"), File(rootDir, "x"), rootDir.resolve("x"), layout.settingsDirectory.file("x")) is relative to it.
var rootDirIdents = map[string]bool{"rootProject": true, "rootDir": true, "settingsDir": true, "settingsDirectory": true}

// scriptRefs is what a build script says about other projects and files.
type scriptRefs struct {
	refs []gref
	// configures are the scopes of the blocks that configure other projects (allprojects/subprojects/project(":x") { }): those
	// projects depend on the script.
	configures []scopeInfo
	// cross is non-empty when the script uses a hook whose effect on other projects cannot be modelled (beforeProject, ...).
	cross string
}

func (m *gradleModel) scriptRefs(p string) *scriptRefs {
	if c, ok := m.refs[p]; ok {
		return c
	}
	sr := &scriptRefs{}
	m.refs[p] = sr
	src, ok := readScript(m.t, p)
	if !ok {
		sr.refs = append(sr.refs, gref{kind: refUnplaceable, val: "script " + p + " is unreadable"})
		sr.cross = "unreadable script"
		return sr
	}
	toks, ok := lexGradle(src, strings.HasSuffix(p, ".kts"))
	if !ok {
		sr.refs = append(sr.refs, gref{kind: refUnplaceable, val: "script " + p + " cannot be tokenized"})
		sr.cross = "script cannot be tokenized"
		return sr
	}
	sr.scan(toks)
	return sr
}

// workDirRef reads `workingDir = rootDir`, `workingDir(rootProject.projectDir)`, `workingDir = file("../x")` at toks[i]: the root
// directory or a literal directory. Other values (build directories, variables) are not modelled.
func workDirRef(toks []token, i int) (gref, bool) {
	for j := i + 1; j < len(toks) && j <= i+8 && toks[j].line == toks[i].line; j++ {
		t := toks[j]
		if isPunct(t, ";") || isPunct(t, "{") || isPunct(t, "}") {
			break
		}
		if t.kind == tIdent && rootDirIdents[t.text] {
			// rootDir, rootProject.projectDir, rootProject.layout.projectDirectory, or a literal below them.
			for k := j + 1; k < len(toks) && k <= j+8 && toks[k].line == t.line; k++ {
				if toks[k].kind == tStr && !toks[k].dyn {
					return gref{kind: refWorkDir, val: cleanRel(toks[k].text), rootRel: true, line: t.line}, true
				}
			}
			return gref{kind: refWorkDir, val: ".", rootRel: true, line: t.line}, true
		}
		if t.kind == tStr {
			s := strings.TrimSuffix(t.text, "/") + "/" // "$rootDir" names the root like "$rootDir/"
			if r, ok := stripRootDir(s); ok {
				return gref{kind: refWorkDir, val: cleanRel(r), rootRel: r != s, line: t.line}, true
			}
			return gref{}, false
		}
	}
	return gref{}, false
}

// extraStatement: the tokens of the statement before toks[end] (on its line) name an extra-properties container.
func extraStatement(toks []token, start, end int) bool {
	if end >= len(toks) {
		end = len(toks) - 1
	}
	for j := end - 1; j >= start && j >= 0; j-- {
		if toks[j].line != toks[end].line {
			break
		}
		if isIdent(toks[j], "ext") || isIdent(toks[j], "extra") {
			return true
		}
	}
	return false
}

// headScope decides what kind of block a '{' opens from the tokens of its statement head. cross is non-empty for hooks whose
// effect on other projects cannot be modelled (beforeProject, afterProject, projectsEvaluated).
func headScope(head []token) (sc scopeInfo, cross string, ok bool) {
	sawConfigure := false
	for i, t := range head {
		if t.kind != tIdent {
			continue
		}
		prevDot := i > 0 && isPunct(head[i-1], ".")
		switch t.text {
		case "allprojects":
			if prevDot {
				return scopeInfo{kind: scopeBuild}, "", true // rootProject.allprojects { }: every project of the build
			}
			return scopeInfo{kind: scopeAll}, "", true
		case "subprojects":
			if prevDot {
				return scopeInfo{kind: scopeBuild}, "", true
			}
			return scopeInfo{kind: scopeSub}, "", true
		case "beforeProject", "afterProject", "projectsEvaluated", "projectsLoaded":
			return scopeInfo{kind: scopeBuild}, t.text, true
		case "rootProject":
			if i+1 == len(head) {
				return scopeInfo{kind: scopeProject, arg: ":"}, "", true // rootProject { } / gradle.rootProject { }
			}
		case "configure":
			if i+1 < len(head) && isPunct(head[i+1], "(") {
				sawConfigure = true
			}
		}
	}
	if sawConfigure {
		return scopeInfo{kind: scopeBuild}, "", true
	}
	// project(":x") { ... } at the start of a statement (optionally through rootProject.).
	i := 0
	if len(head) >= 3 && isIdent(head[0], "rootProject") && isPunct(head[1], ".") {
		i = 2
	}
	if i < len(head) && isIdent(head[i], "project") && i+1 < len(head) && isPunct(head[i+1], "(") {
		if v, _, ok := projectArg(head, i); ok {
			return scopeInfo{kind: scopeProject, arg: v}, "", true
		}
		return scopeInfo{kind: scopeBuild}, "", true
	}
	return scopeInfo{}, "", false
}

// projectArg reads the literal path argument of a call toks[i]( ... ): `("x")`, `(path: "x", ...)`, `(path = "x")`.
func projectArg(toks []token, i int) (val string, next int, ok bool) {
	j := i + 2
	if j >= len(toks) {
		return "", j, false
	}
	if toks[j].kind == tStr {
		return toks[j].text, j + 1, !toks[j].dyn
	}
	if isIdent(toks[j], "path") && j+2 < len(toks) && (isPunct(toks[j+1], ":") || isPunct(toks[j+1], "=")) && toks[j+2].kind == tStr {
		return toks[j+2].text, j + 3, !toks[j+2].dyn
	}
	return "", j, false
}

func (sr *scriptRefs) scan(toks []token) {
	stack := []scopeInfo{{kind: scopeSelf}}
	// exported[k]: block k stores values into extra properties (ext { } / extra.apply { }).
	exported := []bool{false}
	stmtStart := 0
	cur := func() scopeInfo { return stack[len(stack)-1] }
	var i int
	add := func(r gref) {
		r.sc = cur()
		if exported[len(exported)-1] || extraStatement(toks, stmtStart, i) {
			// A value stored in an extra property (ext.core = project(":core"), val core by extra(project(":core"))) is
			// inherited by, and can be used from, any project of the build.
			r.sc = scopeInfo{kind: scopeBuild}
		}
		sr.refs = append(sr.refs, r)
	}
	for i = 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case isPunct(t, "{"):
			head := toks[stmtStart:i]
			for len(head) > 0 && head[0].line < t.line-1 {
				head = head[1:]
			}
			sc := cur()
			if s, why, ok := headScope(head); ok {
				sc = s
				sr.configures = append(sr.configures, s)
				if why != "" && sr.cross == "" {
					sr.cross = why
				}
			}
			stack = append(stack, sc)
			exported = append(exported, exported[len(exported)-1] || extraStatement(toks, stmtStart, i))
			stmtStart = i + 1
		case isPunct(t, "}"):
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
				exported = exported[:len(exported)-1]
			}
			stmtStart = i + 1
		case isPunct(t, ";"):
			stmtStart = i + 1
		case t.kind == tIdent:
			nextDot := i+1 < len(toks) && isPunct(toks[i+1], ".")
			switch {
			case t.text == "rootProject" && !nextDot && !(i+1 < len(toks) && isPunct(toks[i+1], "{")):
				// rootProject used as a value: implementation(rootProject), testImplementation rootProject.
				add(gref{kind: refPath, val: ":", line: t.line})
			case projectSets[t.text] && nextDot, t.text == "getAllprojects" || t.text == "getSubprojects" || t.text == "getChildProjects",
				t.text == "evaluationDependsOnChildren":
				add(gref{kind: refUnplaceable, val: t.text + " used as a collection at line " + itoa(t.line), line: t.line})
			}
			if t.text == "workingDir" || t.text == "setWorkingDir" {
				if r, ok := workDirRef(toks, i); ok {
					add(r)
				}
			}
			switch t.text {
			case "beforeProject", "afterProject", "projectsEvaluated":
				if sr.cross == "" {
					sr.cross = t.text
				}
			case "project", "findProject", "evaluationDependsOn":
				if i+1 >= len(toks) || !isPunct(toks[i+1], "(") {
					continue
				}
				if i > 0 && (isIdent(toks[i-1], "fun") || isIdent(toks[i-1], "def")) {
					continue
				}
				if v, _, ok := projectArg(toks, i); ok {
					add(gref{kind: refPath, val: v, line: t.line, strict: t.text != "findProject"})
				} else {
					add(gref{kind: refUnplaceable, val: t.text + "(...) at line " + itoa(t.line), line: t.line})
				}
			case "projects":
				if i > 0 && isPunct(toks[i-1], ".") {
					continue
				}
				var segs []string
				j := i + 1
				for j+1 < len(toks) && isPunct(toks[j], ".") && toks[j+1].kind == tIdent {
					segs = append(segs, toks[j+1].text)
					j += 2
				}
				if len(segs) > 0 {
					add(gref{kind: refAccessor, val: strings.Join(segs, "."), line: t.line})
				}
			case "apply":
				sr.scanApply(toks, i, add)
			}
		case t.kind == tStr:
			sr.scanString(toks, i, add)
		}
	}
}

func (sr *scriptRefs) scanApply(toks []token, i int, add func(gref)) {
	for j := i + 1; j < len(toks) && j < i+4; j++ {
		if !isIdent(toks[j], "from") {
			continue
		}
		rootRel := false
		for k := j + 1; k < len(toks) && k < j+10; k++ {
			if isIdent(toks[k], "rootProject") || isIdent(toks[k], "rootDir") {
				rootRel = true
			}
			if toks[k].kind != tStr {
				continue
			}
			s := toks[k].text
			if strings.Contains(s, "://") {
				return
			}
			if r, ok := stripRootDir(s); ok {
				if r != s {
					rootRel = true
				}
				if strings.HasPrefix(r, "$") {
					break
				}
				add(gref{kind: refApply, val: cleanRel(r), rootRel: rootRel, line: toks[k].line})
				return
			}
			for _, pre := range []string{"${projectDir}/", "$projectDir/", "${project.projectDir}/"} {
				if r, found := strings.CutPrefix(s, pre); found {
					if ok2 := !strings.Contains(r, "$"); ok2 {
						add(gref{kind: refApply, val: cleanRel(r), line: toks[k].line})
						return
					}
				}
			}
			break
		}
		add(gref{kind: refUnplaceable, val: "apply from a non-literal script at line " + itoa(toks[i].line), line: toks[i].line})
		return
	}
}

// afterRootDir: the string toks[k] follows a root-directory identifier in the same call chain on its line.
func afterRootDir(toks []token, k int) bool {
	for j := k - 1; j >= 0 && j >= k-10; j-- {
		t := toks[j]
		if t.line != toks[k].line || isPunct(t, ";") || isPunct(t, "{") || isPunct(t, "}") || isPunct(t, "=") {
			return false
		}
		if t.kind == tIdent && rootDirIdents[t.text] {
			return true
		}
	}
	return false
}

func (sr *scriptRefs) scanString(toks []token, k int, add func(gref)) {
	t := toks[k]
	s := t.text
	if n := len(sr.refs); n > 0 && sr.refs[n-1].line == t.line {
		// The argument of the project(...) / apply from: call just recorded.
		if last := sr.refs[n-1]; (last.kind == refPath || last.kind == refApply) && (last.val == s || strings.HasSuffix(s, "/"+last.val)) {
			return
		}
	}
	if !t.dyn && len(s) >= 2 && s[0] == ':' && (s[1] == '_' || s[1] >= 'a' && s[1] <= 'z' || s[1] >= 'A' && s[1] <= 'Z' || s[1] >= '0' && s[1] <= '9') {
		add(gref{kind: refStrPath, val: s, line: t.line})
		return
	}
	if r, ok := stripRootDir(s); ok && r != s && r != "" {
		add(gref{kind: refFile, val: cleanRel(r), rootRel: true, line: t.line})
		return
	}
	for _, pre := range []string{"${projectDir}/", "$projectDir/", "${project.projectDir}/"} {
		if r, found := strings.CutPrefix(s, pre); found {
			s = r
			break
		}
	}
	if strings.HasPrefix(s, "../") || strings.HasPrefix(s, "..\\") {
		if !strings.Contains(s, "$") {
			add(gref{kind: refFile, val: cleanRel(s), line: t.line})
		}
		return
	}
	if !t.dyn && s != "" && !strings.ContainsAny(s, "$:*?\n") && afterRootDir(toks, k) {
		add(gref{kind: refFile, val: cleanRel(s), rootRel: true, mustExist: true, line: t.line})
	}
}

// resolveStrPath maps ":a:b:task" to the project with the longest matching path (never the root project).
func (gb *gbuild) resolveStrPath(s string) []*gproj {
	segs := strings.Split(strings.TrimPrefix(s, ":"), ":")
	for n := len(segs); n >= 1; n-- {
		if out := gb.lookup(":"+strings.Join(segs[:n], ":"), nil); len(out) > 0 {
			return out
		}
	}
	return nil
}

var _ = path.Join
