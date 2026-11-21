package runner

import (
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
	for _, t := range tests {
		_, _, pp, ok := jvm.ParseUnitID(t.Unit)
		if !ok {
			pp = t.Package
		}
		if pp == "" || pp == ":" {
			tasks = append(tasks, ":test")
		} else {
			tasks = append(tasks, pp+":test")
		}
	}
	cmds, bn := batch(jvm.RunnerGradle, k.root, []string{tool}, dedupe(tasks), maxArgs(opt))
	return cmds, append([]string{gradleNote}, bn...)
}
