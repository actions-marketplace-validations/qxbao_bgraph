package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/model"
)

const (
	gradleNote = "gradle: a remote build cache already avoids re-running unchanged tests; here the value is skipping configuration and compilation of unaffected projects. " +
		"A project without a `test` task (no java plugin) makes `:<project>:test` fail"
)

// gradleCommands: `gradlew :a:test :b:test` in the build directory.
func gradleCommands(k jvmKey, tests []model.SelectedTest, opt Options) ([]Command, []string) {
	tool := launcher(k.root, k.wrapper, "gradle", ".bat")
	var tasks []string
	filters := map[string][]string{}
	var notes []string
	for _, t := range tests {
		_, _, pp, ok := jvm.ParseUnitID(t.Unit)
		if !ok {
			pp = t.Package
		}
		task := pp + ":test"
		if pp == "" || pp == ":" {
			pp, task = ":", ":test"
		}
		switch {
		case t.ClassFilter && opt.ListDir == "":
			notes = append(notes, "gradle: the dynamic class layer narrowed "+pp+" but a class filter needs an init script (--list-dir): running all its tests")
		case t.ClassFilter && len(t.Classes) == 0:
			// The class layer kept no class: compile the tests, run nothing (docs/dynamic.md §9.1).
			task = strings.TrimSuffix(task, "test") + "testClasses"
		case t.ClassFilter:
			filters[pp] = append(filters[pp], t.Classes...)
		}
		tasks = append(tasks, task)
	}
	prefix := []string{tool}
	if len(filters) > 0 {
		file, err := gradleFilterScript(opt.ListDir, k.root, filters)
		if err != nil {
			return nil, []string{"gradle: cannot write the class filter init script: " + err.Error()}
		}
		prefix = append(prefix, "-I", file)
	}
	cmds, bn := batch(jvm.RunnerGradle, k.root, prefix, dedupe(tasks), maxArgs(opt))
	for i := range cmds {
		if len(filters) > 0 {
			cmds[i].ListFile = prefix[2]
		}
	}
	return cmds, append(append([]string{gradleNote}, notes...), bn...)
}

// gradleFilterScript writes an init script that restricts the Test tasks of each narrowed project to its kept classes and
// their nested classes (docs/dynamic.md §9.6), with failOnNoMatchingTests = false (a kept type of a changed test file may be
// no test class).
func gradleFilterScript(dir, root string, filters map[string][]string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("// bgraph: test classes kept by the dynamic class layer (docs/dynamic.md §9.6)\ndef bgraphFilters = [\n")
	paths := make([]string, 0, len(filters))
	for p := range filters {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		var qs []string
		for _, c := range dedupe(filters[p]) {
			qs = append(qs, groovyString(c), groovyString(c+"$*"))
		}
		fmt.Fprintf(&b, "  %s: [%s],\n", groovyString(p), strings.Join(qs, ", "))
	}
	b.WriteString("]\nallprojects { p ->\n  def f = bgraphFilters[p.path]\n  if (f != null) {\n    p.tasks.withType(Test).configureEach { t ->\n" +
		"      t.filter.failOnNoMatchingTests = false\n      f.each { t.filter.includeTestsMatching(it) }\n    }\n  }\n}\n")
	file := filepath.Join(dir, "gradle-classes-"+sanitize(root)+".gradle")
	return file, os.WriteFile(file, []byte(b.String()), 0o644)
}

// groovyString quotes s as a single-quoted Groovy string (no interpolation).
func groovyString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(s) + "'"
}
