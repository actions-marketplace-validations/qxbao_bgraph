// Package classify holds the built-in global widening table and the list of harmless files (docs/architecture.md, "Global inputs and harmless files").
//
// The table is data (a slice of {pattern, category}) so it is easy to inspect. Rules in bgraph.toml can override this table at the
// caller level (classify does not read bgraph.toml): the caller checks rules first and calls Global only when no rule matches.
package classify

import (
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
)

type entry struct {
	pattern  string
	category string
}

// globalTable: a pattern "**/x" matches at any depth; a pattern without a leading "**/" matches only at the root (convention of the glob package).
var globalTable = []entry{
	// CI
	{".github/workflows/**", "ci"},
	{".github/actions/**", "ci"},
	{".gitlab-ci.yml", "ci"},
	{".gitlab/ci/**", "ci"},
	{"**/azure-pipelines*.yml", "ci"},
	{".circleci/**", "ci"},
	{"**/Jenkinsfile*", "ci"},
	{".buildkite/**", "ci"},
	{"bitbucket-pipelines.yml", "ci"},
	{".travis.yml", "ci"},
	{"appveyor.yml", "ci"},
	// container
	{"**/Dockerfile*", "container"},
	{"**/*.dockerfile", "container"},
	{"**/.dockerignore", "container"},
	{"**/docker-compose*.y*ml", "container"},
	{"**/compose*.y*ml", "container"},
	// script build
	{"**/Makefile", "build_script"},
	{"**/GNUmakefile", "build_script"},
	{"**/*.mk", "build_script"},
	{"**/justfile", "build_script"},
	{"**/Justfile", "build_script"},
	{"**/.justfile", "build_script"},
	{"**/Taskfile.y*ml", "build_script"},
	// environment
	{"**/.env", "env"},
	{"**/.env.*", "env"},
	{"**/.envrc", "env"},
	// toolchain pins
	{"**/.nvmrc", "toolchain_pin"},
	{"**/.node-version", "toolchain_pin"},
	{"**/.python-version", "toolchain_pin"},
	{"**/rust-toolchain", "toolchain_pin"},
	{"**/rust-toolchain.toml", "toolchain_pin"},
	{"**/global.json", "toolchain_pin"},
	{"**/.tool-versions", "toolchain_pin"},
	{"**/mise.toml", "toolchain_pin"},
	{"**/.mise.toml", "toolchain_pin"},
	{"**/.sdkmanrc", "toolchain_pin"},
	{"**/.java-version", "toolchain_pin"},
	{"**/.ruby-version", "toolchain_pin"},
	{"**/go.work", "toolchain_pin"},
	{"**/go.work.sum", "toolchain_pin"},
	// workspace definitions
	{"**/pnpm-workspace.yaml", "workspace_def"},
	{"**/lerna.json", "workspace_def"},
	{"**/nx.json", "workspace_def"},
	{"**/turbo.json", "workspace_def"},
	{"rush.json", "workspace_def"},
	{".gitmodules", "workspace_def"},
	// git attributes
	{"**/.gitattributes", "git_attributes"},
	// package manager configuration
	{"**/.npmrc", "package_manager_config"},
	{"**/.yarnrc", "package_manager_config"},
	{"**/.yarnrc.yml", "package_manager_config"},
	{"**/.pnpmfile.cjs", "package_manager_config"},
	{"**/bunfig.toml", "package_manager_config"},
	{"**/.yarn/**", "package_manager_config"},
}

var harmlessPatterns = []string{
	"LICENSE*", "COPYING*", "NOTICE*",
	"**/.gitignore",
	"CODEOWNERS", ".github/CODEOWNERS", "docs/CODEOWNERS",
	".github/ISSUE_TEMPLATE/**",
	".github/PULL_REQUEST_TEMPLATE*",
	".github/PULL_REQUEST_TEMPLATE/**",
	".github/FUNDING.yml",
	".mailmap",
}

var docPatterns = []string{"**/*.md", "**/*.markdown", "**/*.rst", "**/*.adoc"}

type compiled struct {
	p        *glob.Pattern
	category string
}

var (
	globalC   []compiled
	harmlessC []*glob.Pattern
	docC      []*glob.Pattern
)

func init() {
	for _, e := range globalTable {
		globalC = append(globalC, compiled{glob.MustCompile(e.pattern), e.category})
	}
	for _, s := range harmlessPatterns {
		harmlessC = append(harmlessC, glob.MustCompile(s))
	}
	for _, s := range docPatterns {
		docC = append(docC, glob.MustCompile(s))
	}
}

// Global returns the widening (Tier full) if path is a whole-repo input.
func Global(p string) (model.Widening, bool) {
	for _, c := range globalC {
		if c.p.Match(p) {
			return model.Widening{
				Tier:   model.TierFull,
				Reason: model.ReasonGlobalInput,
				Path:   p,
				Detail: c.category,
			}, true
		}
	}
	return model.Widening{}, false
}

// Harmless: files provably not affecting tests.
func Harmless(p string) bool {
	for _, h := range harmlessC {
		if h.Match(p) {
			return true
		}
	}
	return false
}

// Doc: documentation (.md/.markdown/.rst/.adoc, any depth). The caller may skip it only when the file is not a graph node.
func Doc(p string) bool {
	for _, d := range docC {
		if d.Match(p) {
			return true
		}
	}
	return false
}
