package evalkit

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/cpp"
	"github.com/qxbao/bgraph/internal/scancache"
)

type CPPProviderResult struct {
	Repo         string   `json:"repo"`
	Commit       string   `json:"commit"`
	System       string   `json:"system"`
	Project      string   `json:"project"`
	Image        string   `json:"image"`
	Mode         string   `json:"mode,omitempty"`
	NativeNames  []string `json:"native_names,omitempty"`
	StaticNames  []string `json:"static_names,omitempty"`
	NativeInputs int      `json:"native_test_source_links,omitempty"`
	Mismatches   []string `json:"mismatches,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type cppNativeTest struct {
	Name    string
	Depends []string
	Command []string
}

type cppMesonTarget struct {
	ID       string
	Filename []string
	Sources  []struct {
		Sources []string
	} `json:"target_sources"`
}

type cppCMakeObject struct {
	Kind     string
	JSONFile string
}

type cppCMakeTarget struct {
	Name      string
	Artifacts []struct{ Path string }
	Sources   []struct {
		Path        string
		IsGenerated bool
	}
}

func cppReadJSON(file string, target any) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func cppCMakeProviders(dir string) (map[string]cppCMakeTarget, error) {
	reply := filepath.Join(dir, "build", ".cmake", "api", "v1", "reply")
	matches, err := filepath.Glob(filepath.Join(reply, "index-*.json"))
	if err != nil {
		return nil, err
	}
	if len(matches) != 1 {
		return nil, errors.New("CMake file API index missing or ambiguous")
	}
	var index struct{ Objects []cppCMakeObject }
	if err := cppReadJSON(matches[0], &index); err != nil {
		return nil, err
	}
	out := map[string]cppCMakeTarget{}
	for _, object := range index.Objects {
		if object.Kind != "codemodel" {
			continue
		}
		var model struct {
			Configurations []struct {
				Targets []cppCMakeObject
			}
		}
		if err := cppReadJSON(filepath.Join(reply, filepath.Base(object.JSONFile)), &model); err != nil {
			return nil, err
		}
		for _, config := range model.Configurations {
			for _, descriptor := range config.Targets {
				var target cppCMakeTarget
				if err := cppReadJSON(filepath.Join(reply, filepath.Base(descriptor.JSONFile)), &target); err != nil {
					return nil, err
				}
				for _, output := range target.Artifacts {
					for _, buildDir := range []string{"/tmp/build", "/out/build"} {
						out[path.Clean(path.Join(buildDir, output.Path))] = target
					}
				}
			}
		}
	}
	return out, nil
}

func cppNativeSources(dir string, result CPPProviderResult) (map[string][]string, error) {
	output := filepath.Join(dir, "tests.json")
	sources := map[string][]string{}
	if result.System == cpp.CMake {
		var info struct{ Tests []cppNativeTest }
		if err := cppReadJSON(output, &info); err != nil {
			return nil, err
		}
		targets, err := cppCMakeProviders(dir)
		if err != nil {
			return nil, err
		}
		for _, test := range info.Tests {
			sources[test.Name] = nil
			for _, command := range test.Command {
				for _, input := range targets[command].Sources {
					if input.IsGenerated {
						continue
					}
					file := input.Path
					if rel, ok := strings.CutPrefix(file, "/tmp/source/"); ok {
						file = rel
					} else if !path.IsAbs(file) {
						file = path.Join(result.Project, file)
					}
					if cppOracleSource(file) && !path.IsAbs(file) {
						sources[test.Name] = append(sources[test.Name], file)
					}
				}
			}
		}
		return sources, nil
	}

	var tests []cppNativeTest
	if err := cppReadJSON(output, &tests); err != nil {
		return nil, err
	}
	var targets []cppMesonTarget
	if err := cppReadJSON(filepath.Join(dir, "targets.json"), &targets); err != nil {
		return nil, err
	}
	byID := map[string][]string{}
	byPath := map[string][]string{}
	for _, target := range targets {
		for _, part := range target.Sources {
			for _, source := range part.Sources {
				if file, ok := strings.CutPrefix(source, "/tmp/source/"); ok && cppOracleSource(file) {
					byID[target.ID] = append(byID[target.ID], file)
				}
			}
		}
		for _, file := range target.Filename {
			byPath[file] = append(byPath[file], byID[target.ID]...)
		}
	}
	for _, test := range tests {
		sources[test.Name] = nil
		for _, id := range test.Depends {
			sources[test.Name] = append(sources[test.Name], byID[id]...)
		}
		if len(test.Command) > 0 {
			sources[test.Name] = append(sources[test.Name], byPath[test.Command[0]]...)
		}
	}
	return sources, nil
}

// CompareCPPProviders checks native test names and each resolved test source.
// Whole-project units cover a nonempty native suite at the same root and in
// the same build system without asserting static per-test source ownership.
func CompareCPPProviders(ctx context.Context, work, nativeRoot string) ([]CPPProviderResult, error) {
	var results []CPPProviderResult
	err := filepath.WalkDir(nativeRoot, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name() == "build" && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.Name() != "provenance.json" || entry.IsDir() {
			return nil
		}
		var result CPPProviderResult
		if err := cppReadJSON(file, &result); err != nil {
			return err
		}
		if result.Error == "" {
			result = cppCompareProvider(ctx, work, filepath.Dir(file), result)
		}
		results = append(results, result)
		return nil
	})
	return results, err
}

func cppCompareProvider(ctx context.Context, work, nativeDir string, result CPPProviderResult) CPPProviderResult {
	native, err := cppNativeSources(nativeDir, result)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	for name, files := range native {
		result.NativeNames = append(result.NativeNames, name)
		result.NativeInputs += len(uniqSorted(files))
	}
	result.NativeNames = uniqSorted(result.NativeNames)
	t, err := newGitTreeWant(ctx, RustRepoDir(work, result.Repo), result.Commit, cppOracleInput)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer t.Close()
	a, err := cpp.New().Analyze(ctx, t, scancache.New())
	if err != nil {
		result.Error = err.Error()
		return result
	}
	units, owners := cppTestUnits(a)
	root := cppProject{result.System, result.Project}
	static := units[root]
	if static[""] != "" {
		result.Mode = "whole"
		if len(result.NativeNames) == 0 {
			result.Mode = "whole-empty-native-suite"
		}
		return result
	}
	result.Mode = "named"
	for name := range static {
		result.StaticNames = append(result.StaticNames, name)
		if _, ok := native[name]; !ok {
			result.Mismatches = append(result.Mismatches, "extra static test "+name)
		}
	}
	for name, files := range native {
		id := static[name]
		if id == "" {
			result.Mismatches = append(result.Mismatches, "missing test "+name)
			continue
		}
		for _, file := range uniqSorted(files) {
			if !containsStr(owners[file], id) {
				result.Mismatches = append(result.Mismatches, name+" does not own "+file)
			}
		}
	}
	result.StaticNames = uniqSorted(result.StaticNames)
	result.Mismatches = uniqSorted(result.Mismatches)
	return result
}
