// Package dotnetdyn is the .NET part of the dynamic layer (docs/dynamic-layer.md §9): per-test-class records made from the code
// coverage of one fresh test host per class (Microsoft.CodeCoverage, Cobertura), the guards that decide whether a selection may be
// narrowed below the MSBuild project, and the narrowing itself (a selected test project runs only the test classes whose
// records reach a changed file).
package dotnetdyn

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/tree"
)

// Eco is the ecosystem name of the records.
const Eco = "dotnet"

// CollectorID identifies the collector and its record format (docs/dynamic-layer.md §9.3).
const CollectorID = "dotnet-cov-1"

// BuildFile reports whether repo path p is part of the build definition fingerprinted by Fingerprint: MSBuild project, props and
// targets files, central package management, global.json, NuGet configuration, lock files and run settings.
func BuildFile(p string) bool {
	q := "/" + strings.ToLower(p)
	if strings.Contains(q, "/obj/") || strings.Contains(q, "/bin/") {
		return false
	}
	base := path.Base(strings.ToLower(p))
	switch path.Ext(base) {
	case ".csproj", ".fsproj", ".vbproj", ".props", ".targets", ".runsettings", ".rsp":
		return true
	}
	switch base {
	case "global.json", "nuget.config", "packages.lock.json", "dotnet-tools.json":
		return true
	}
	return false
}

// SDK describes the .NET SDK the recording and the narrowed runs use: `dotnet --version` (global.json applies) and the installed
// runtimes, run in the repository root. env overrides the process environment (DOTNET_ROOT, PATH...).
func SDK(ctx context.Context, root string, env []string) ([]string, error) {
	var out []string
	for _, args := range [][]string{{"--version"}, {"--list-runtimes"}} {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		cmd := exec.CommandContext(c, "dotnet", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), append([]string{"DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1"}, env...)...)
		b, err := cmd.Output()
		cancel()
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(strings.ReplaceAll(string(bytes.TrimSpace(b)), "\r\n", "\n"), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, args[0]+" "+l)
			}
		}
	}
	return out, nil
}

// Fingerprint is the .NET environment fingerprint: the collector, OS and architecture, the SDK and runtimes (SDK), the extra
// dotnet arguments of the recording (a configuration or property changes what is compiled; the narrowed commands run without
// any, so only records made without them are used) and the content of every build definition file of t (BuildFile).
func Fingerprint(sdk, buildArgs []string, t tree.Tree) string {
	interp := append([]string{"os " + runtime.GOOS, "arch " + runtime.GOARCH}, sdk...)
	if len(buildArgs) > 0 {
		interp = append(interp, "build-args "+strings.Join(buildArgs, " "))
	}
	files := map[string]string{}
	for _, p := range t.Files() {
		if BuildFile(p) {
			files[p] = t.Key(p)
		}
	}
	return dynamic.Fingerprint(CollectorID, interp, files)
}

var (
	floatingVersion = regexp.MustCompile(`(?i)<Package(?:Reference|Version)\b[^>]*\bVersion(?:Override)?\s*=\s*"\s*([^"]*)"`)
	floatingElem    = regexp.MustCompile(`(?is)<Package(?:Reference|Version)\b[^>]*>.*?<Version(?:Override)?>\s*([^<]*)</Version`)
)

// FloatingVersions returns the first build file of t that declares a floating or ranged NuGet version ("*", "[...]", "(...)"):
// such a version can resolve to another package without any file change, so records are never trusted ("" when none).
func FloatingVersions(t tree.Tree) string {
	for _, p := range t.Files() {
		if !BuildFile(p) {
			continue
		}
		ext := path.Ext(strings.ToLower(p))
		if ext != ".csproj" && ext != ".fsproj" && ext != ".vbproj" && ext != ".props" && ext != ".targets" {
			continue
		}
		b, err := t.Read(p)
		if err != nil {
			return p + " (unreadable)"
		}
		for _, re := range []*regexp.Regexp{floatingVersion, floatingElem} {
			for _, m := range re.FindAllSubmatch(b, -1) {
				v := strings.TrimSpace(string(m[1]))
				if strings.Contains(v, "*") || strings.HasPrefix(v, "[") || strings.HasPrefix(v, "(") {
					return p + ` (version "` + v + `")`
				}
			}
		}
	}
	return ""
}

// unsafePackages are package IDs (prefix match, case-insensitive) whose presence in a test project means its records cannot be
// trusted: they read assemblies or sources as data (the coverage sees no line of what they read), or they occupy the CLR
// profiler slot the coverage needs.
var unsafePackages = []string{
	"microsoft.codeanalysis", "mono.cecil", "system.reflection.metadataloadcontext", "icsharpcode.decompiler",
	"microsoft.qualitytools.testing.fakes", "justmock", "telerik.justmock", "typemock",
}

var packageRef = regexp.MustCompile(`(?i)<(?:PackageReference|Reference)\b[^>]*\bInclude\s*=\s*"([^"]+)"`)

// UnsafeReference returns the first package reference of project file src that makes its records untrustworthy ("" when none).
func UnsafeReference(src []byte) string {
	for _, m := range packageRef.FindAllSubmatch(src, -1) {
		id := strings.ToLower(strings.TrimSpace(string(m[1])))
		for _, u := range unsafePackages {
			if id == u || strings.HasPrefix(id, u+".") || strings.HasPrefix(id, u+",") {
				return string(m[1])
			}
		}
	}
	return ""
}
