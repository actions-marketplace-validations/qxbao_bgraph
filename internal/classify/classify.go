// Package classify chứa bảng nới toàn cục dựng sẵn và danh sách tệp vô hại (mục 6.7 của docs/plan.md).
//
// Bảng là dữ liệu (slice {pattern, category}) để dễ kiểm. Luật trong bgraph.toml có thể ghi đè bảng này ở tầng
// gọi (classify không đọc bgraph.toml): bên gọi kiểm luật trước, chỉ gọi Global khi không có luật khớp.
package classify

import (
	"bgraph/internal/glob"
	"bgraph/internal/model"
)

type entry struct {
	pattern  string
	category string
}

// globalTable: mẫu "**/x" khớp ở mọi độ sâu; mẫu không có "**/" đầu chỉ khớp ở gốc (quy ước của gói glob).
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
	// môi trường
	{"**/.env", "env"},
	{"**/.env.*", "env"},
	{"**/.envrc", "env"},
	// ghim toolchain
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
	// định nghĩa workspace
	{"**/pnpm-workspace.yaml", "workspace_def"},
	{"**/lerna.json", "workspace_def"},
	{"**/nx.json", "workspace_def"},
	{"**/turbo.json", "workspace_def"},
	{"rush.json", "workspace_def"},
	{".gitmodules", "workspace_def"},
	// thuộc tính git
	{"**/.gitattributes", "git_attributes"},
	// cấu hình trình quản lý gói
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

// Global trả phần nới (Tier full) nếu path là đầu vào toàn repo.
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

// Harmless: tệp chứng minh được không ảnh hưởng test.
func Harmless(p string) bool {
	for _, h := range harmlessC {
		if h.Match(p) {
			return true
		}
	}
	return false
}

// Doc: tài liệu (.md/.markdown/.rst/.adoc, mọi độ sâu). Bên gọi chỉ được bỏ qua khi tệp không là nút đồ thị.
func Doc(p string) bool {
	for _, d := range docC {
		if d.Match(p) {
			return true
		}
	}
	return false
}
