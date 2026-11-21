package runner

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

// pytestNote is attached to every plan that uses an @argfile.
const pytestArgfileNote = "pytest: @<file> argument files need pytest >= 8.2 (the command line was too long for a direct list)"

// relFrom returns target (repo-relative) relative to the directory from (repo-relative), with ".." segments when needed.
func relFrom(from, to string) string {
	from, to = cleanProject(from), path.Clean(strings.ReplaceAll(to, "\\", "/"))
	if from == "." {
		return to
	}
	fp, tp := strings.Split(from, "/"), strings.Split(to, "/")
	if to == "." {
		tp = nil
	}
	i := 0
	for i < len(fp) && i < len(tp) && fp[i] == tp[i] {
		i++
	}
	parts := make([]string, 0, len(fp)-i+len(tp)-i)
	for j := i; j < len(fp); j++ {
		parts = append(parts, "..")
	}
	parts = append(parts, tp[i:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

// safeArg keeps a path from being read as an option or an @argfile.
func safeArg(p string) string {
	if strings.HasPrefix(p, "-") || strings.HasPrefix(p, "@") {
		return "./" + p
	}
	return p
}

// pythonCommands turns selected pytest and Django units into commands: one per (project, config).
func pythonCommands(tests []model.SelectedTest, opt Options) ([]Command, []string, error) {
	type key struct{ runner, project, config string }
	groups := map[key][]model.SelectedTest{}
	var keys []key
	for _, t := range tests {
		k := key{t.Runner, cleanProject(t.Project), t.Config}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], t)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.project != b.project {
			return a.project < b.project
		}
		if a.runner != b.runner {
			return a.runner < b.runner
		}
		return a.config < b.config
	})
	var cmds []Command
	var notes []string
	for _, k := range keys {
		var c []Command
		var n []string
		var err error
		if k.runner == "django" {
			c, n = djangoCommands(k.project, k.config, groups[k], opt)
		} else {
			c, n, err = pytestCommands(k.project, k.config, groups[k], opt)
		}
		if err != nil {
			return nil, nil, err
		}
		cmds = append(cmds, c...)
		notes = append(notes, n...)
	}
	return cmds, notes, nil
}

func pytestCommands(project, config string, tests []model.SelectedTest, opt Options) ([]Command, []string, error) {
	prefix := []string{"python", "-m", "pytest"}
	if config != "" {
		prefix = append(prefix, "--rootdir", relFrom(project, path.Dir(config)), "-c", relFrom(project, config))
	} else {
		prefix = append(prefix, "--rootdir", ".")
	}
	var args []string
	whole := false
	for _, t := range tests {
		if t.Whole {
			whole = true
			continue
		}
		files := t.Files
		if len(files) == 0 {
			files = []string{t.Unit}
		}
		for _, f := range files {
			args = append(args, safeArg(relFrom(project, f)))
		}
	}
	if whole {
		return []Command{{Runner: "pytest", Project: project, Argv: prefix,
			Note: "test file set of the project is computed: running the whole project"}}, []string{project + "/pytest: running the whole project suite"}, nil
	}
	args = dedupe(args)
	max := maxArgs(opt)
	size := 0
	for _, a := range prefix {
		size += len(a) + 1
	}
	for _, a := range args {
		size += len(a) + 1
	}
	if size > max && opt.ListDir != "" {
		if err := os.MkdirAll(opt.ListDir, 0o755); err != nil {
			return nil, nil, err
		}
		file := filepath.Join(opt.ListDir, "pytest-"+sanitize(project)+".txt")
		if err := os.WriteFile(file, []byte(strings.Join(args, "\n")+"\n"), 0o644); err != nil {
			return nil, nil, err
		}
		return []Command{{Runner: "pytest", Project: project, Argv: append(prefix, "@"+file), ListFile: file}}, []string{pytestArgfileNote}, nil
	}
	cmds, notes := batch("pytest", project, prefix, args, max)
	return cmds, notes, nil
}

// djangoCommands: `python tests/runtests.py <labels>` (Django's own layout) or `python manage.py test <labels>`; labels are dotted
// module paths relative to the directory the driver imports from.
func djangoCommands(project, driver string, tests []model.SelectedTest, opt Options) ([]Command, []string) {
	var prefix []string
	base := project
	if path.Base(driver) == "manage.py" {
		prefix = []string{"python", relFrom(project, driver), "test"}
	} else {
		prefix = []string{"python", relFrom(project, driver)}
		if path.Base(driver) == "runtests.py" {
			base = path.Dir(driver)
		}
	}
	var labels []string
	for _, t := range tests {
		files := t.Files
		if len(files) == 0 {
			files = []string{t.Unit}
		}
		for _, f := range files {
			rel := relFrom(base, f)
			if strings.HasPrefix(rel, "../") || !strings.HasSuffix(rel, ".py") {
				return []Command{{Runner: "django", Project: project, Argv: prefix, Note: "a test file lies outside the label root: running the whole suite"}},
					[]string{fmt.Sprintf("%s/django: running the whole suite", project)}
			}
			labels = append(labels, strings.ReplaceAll(strings.TrimSuffix(rel, ".py"), "/", "."))
		}
	}
	cmds, notes := batch("django", project, prefix, dedupe(labels), maxArgs(opt))
	return cmds, notes
}
