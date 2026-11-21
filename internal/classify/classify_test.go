package classify

import (
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestGlobal(t *testing.T) {
	yes := map[string]string{
		".github/workflows/ci.yml":         "ci",
		".github/actions/setup/action.yml": "ci",
		".gitlab-ci.yml":                   "ci",
		".gitlab/ci/build.yml":             "ci",
		"azure-pipelines.yml":              "ci",
		"build/azure-pipelines-pr.yml":     "ci",
		".circleci/config.yml":             "ci",
		"Jenkinsfile":                      "ci",
		"ci/Jenkinsfile.nightly":           "ci",
		".buildkite/pipeline.yml":          "ci",
		"bitbucket-pipelines.yml":          "ci",
		".travis.yml":                      "ci",
		"appveyor.yml":                     "ci",
		"Dockerfile":                       "container",
		"services/api/Dockerfile":          "container",
		"services/api/Dockerfile.dev":      "container",
		"app.dockerfile":                   "container",
		"a/.dockerignore":                  "container",
		"docker-compose.yml":               "container",
		"docker-compose.override.yaml":     "container",
		"deploy/compose.prod.yml":          "container",
		"Makefile":                         "build_script",
		"pkg/GNUmakefile":                  "build_script",
		"rules.mk":                         "build_script",
		"a/b/justfile":                     "build_script",
		"Justfile":                         "build_script",
		".justfile":                        "build_script",
		"Taskfile.yml":                     "build_script",
		"x/Taskfile.yaml":                  "build_script",
		".env":                             "env",
		"web/.env.local":                   "env",
		".envrc":                           "env",
		".nvmrc":                           "toolchain_pin",
		"a/.node-version":                  "toolchain_pin",
		".python-version":                  "toolchain_pin",
		"rust-toolchain":                   "toolchain_pin",
		"rust-toolchain.toml":              "toolchain_pin",
		"global.json":                      "toolchain_pin",
		".tool-versions":                   "toolchain_pin",
		"mise.toml":                        "toolchain_pin",
		".mise.toml":                       "toolchain_pin",
		".sdkmanrc":                        "toolchain_pin",
		".java-version":                    "toolchain_pin",
		".ruby-version":                    "toolchain_pin",
		"go.work":                          "toolchain_pin",
		"go.work.sum":                      "toolchain_pin",
		"pnpm-workspace.yaml":              "workspace_def",
		"lerna.json":                       "workspace_def",
		"nx.json":                          "workspace_def",
		"turbo.json":                       "workspace_def",
		"packages/ui/turbo.json":           "workspace_def",
		"rush.json":                        "workspace_def",
		".gitmodules":                      "workspace_def",
		".gitattributes":                   "git_attributes",
		"sub/dir/.gitattributes":           "git_attributes",
		".npmrc":                           "package_manager_config",
		"pkg/.npmrc":                       "package_manager_config",
		".yarnrc":                          "package_manager_config",
		".yarnrc.yml":                      "package_manager_config",
		".pnpmfile.cjs":                    "package_manager_config",
		"bunfig.toml":                      "package_manager_config",
		".yarn/releases/yarn-4.0.0.cjs":    "package_manager_config",
		".yarn/cache/x.zip":                "package_manager_config",
	}
	for p, cat := range yes {
		w, ok := Global(p)
		if !ok {
			t.Errorf("Global(%q): want true", p)
			continue
		}
		if w.Tier != model.TierFull || w.Reason != model.ReasonGlobalInput || w.Path != p || w.Detail != cat {
			t.Errorf("Global(%q) = %+v, want category %s", p, w, cat)
		}
	}
	no := []string{
		"src/makefile.ts",
		"docs/Makefile.md",
		"src/Dockerfile.ts.bak/x.ts",
		"src/index.ts",
		"sub/rush.json",
		"sub/.gitmodules",
		"README.md",
		"src/env.ts",
		"src/.env/readme.txt",
		"pkg/lerna.json.bak",
		"github/workflows/ci.yml",
		"sub/.github/workflows/ci.yml",
		"docs/.yarn",
		"nx.json.md",
		"compose.go",
	}
	for _, p := range no {
		if w, ok := Global(p); ok {
			t.Errorf("Global(%q) = %+v, want false", p, w)
		}
	}
}

func TestHarmless(t *testing.T) {
	yes := []string{
		"LICENSE", "LICENSE.md", "LICENSE-MIT", "COPYING", "COPYING.LESSER", "NOTICE", "NOTICE.txt",
		".gitignore", "a/b/.gitignore",
		"CODEOWNERS", ".github/CODEOWNERS", "docs/CODEOWNERS",
		".github/ISSUE_TEMPLATE/bug.md", ".github/ISSUE_TEMPLATE/config.yml",
		".github/PULL_REQUEST_TEMPLATE.md", ".github/PULL_REQUEST_TEMPLATE/x.md",
		".github/FUNDING.yml", ".mailmap",
	}
	for _, p := range yes {
		if !Harmless(p) {
			t.Errorf("Harmless(%q): want true", p)
		}
	}
	no := []string{
		".editorconfig", "src/LICENSE", "src/CODEOWNERS", "a/.mailmap", "src/index.ts",
		".github/workflows/ci.yml", ".github/FUNDING.yaml", "LICENSES/x",
	}
	for _, p := range no {
		if p == "LICENSES/x" {
			continue // LICENSE* matches only one segment, "LICENSES/x" has two: checked separately below
		}
		if Harmless(p) {
			t.Errorf("Harmless(%q): want false", p)
		}
	}
	if Harmless("LICENSES/x") {
		t.Error("LICENSES/x is not harmless")
	}
	// harmless and global do not intersect
	for _, p := range yes {
		if _, ok := Global(p); ok {
			t.Errorf("%q is both harmless and global", p)
		}
	}
}

func TestDoc(t *testing.T) {
	yes := []string{"README.md", "docs/a/b.md", "x.markdown", "a/b.rst", "guide.adoc"}
	no := []string{"src/a.ts", "md", "a/.md.bak", "docs/Makefile", "notes.mdx", "a.txt"}
	for _, p := range yes {
		if !Doc(p) {
			t.Errorf("Doc(%q): want true", p)
		}
	}
	for _, p := range no {
		if Doc(p) {
			t.Errorf("Doc(%q): want false", p)
		}
	}
}
