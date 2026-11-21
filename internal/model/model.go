// Package model chứa kiểu dữ liệu dùng chung giữa các gói của bgraph (mục 4.2 của docs/plan.md).
//
// Quy ước đường dẫn: mọi đường dẫn là tương đối gốc repo, gạch xuôi, đã path.Clean, không có "./" đầu; gốc repo là ".".
package model

// Tier là phạm vi của phần chọn, từ hẹp đến rộng.
type Tier string

const (
	TierNone    Tier = "none"    // không test nào bị ảnh hưởng
	TierGraph   Tier = "graph"   // phần chọn chứng minh được bằng đồ thị
	TierPackage Tier = "package" // nới tới thư mục / gói / Cargo target
	TierModule  Tier = "module"  // nới tới dự án (package.json, crate, module Maven, project .NET)
	TierFull    Tier = "full"    // chạy toàn bộ
)

// Rank: thứ tự để lấy mức rộng nhất.
func (t Tier) Rank() int {
	switch t {
	case TierNone:
		return 0
	case TierGraph:
		return 1
	case TierPackage:
		return 2
	case TierModule:
		return 3
	}
	return 4
}

// Max trả mức rộng hơn.
func Max(a, b Tier) Tier {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// Lý do nới (mã ổn định, máy đọc được). Giữ tương thích với bint/internal/affected và thêm lý do của mục 7.1.
const (
	ReasonUnknownImport   = "unknown_import"
	ReasonDynamicImport   = "dynamic_import"
	ReasonAmbiguousImport = "ambiguous_import"
	ReasonUncertainScan   = "uncertain_scan"
	ReasonConfigIsCode    = "config_is_code"
	ReasonManifestChanged = "manifest_changed"
	ReasonTestConfig      = "test_config_changed"
	ReasonLockChanged     = "lock_changed"
	ReasonGlobalInput     = "global_input"
	ReasonNoBase          = "no_base"
	ReasonDiffTooLarge    = "diff_too_large"
	ReasonCaseMismatch    = "case_mismatch"
	ReasonUserRule        = "user_rule"
	ReasonUnknownFile     = "unknown_file" // tệp không thuộc hệ sinh thái nào và không ai tham chiếu
	// tệp mã/dữ liệu của một hệ sinh thái mà không ai trong đồ thị phụ thuộc vào (fixture test đọc qua fs, script/bin
	// test chạy qua child_process, điểm vào): không biết ai dùng → nới tới dự án.
	ReasonUnreferenced    = "unreferenced_file"
	ReasonUnsupported     = "unsupported_language"
	ReasonGeneratedOutput = "generated_output" // import tới đầu ra bị ignore
	ReasonFrameworkMagic  = "framework_magic"  // Next file routing, auto-import...
	ReasonSymlinkOutside  = "symlink_outside"
	ReasonSubmodule       = "submodule"
	ReasonInternalError   = "internal_error" // lỗi của chính bgraph: luôn nới, không bao giờ trả rỗng
	ReasonRuleRejected    = "rule_rejected"  // luật affects="none" khớp tệp là nút đồ thị: bị bỏ qua (chỉ cảnh báo, không nới)
)

// Widening ghi một lần nới. Path là tệp gây nới; Scope là phạm vi bị nới (thư mục gói/dự án; rỗng khi Tier=full).
type Widening struct {
	Tier    Tier   `json:"tier"`
	Reason  string `json:"reason"`
	Path    string `json:"path,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Suggest string `json:"suggest,omitempty"` // luật bgraph.toml gợi ý để thu hẹp
}

// Role là vai trò của tệp.
type Role string

const (
	RoleSource    Role = "source"
	RoleTest      Role = "test"
	RoleConfig    Role = "config"
	RoleManifest  Role = "manifest"
	RoleLock      Role = "lock"
	RoleData      Role = "data"
	RoleGenerated Role = "generated"
	RoleUnknown   Role = "unknown"
)

// ImportKind là dạng cú pháp của một tham chiếu import do bộ quét tìm thấy.
type ImportKind string

const (
	ImportStatic      ImportKind = "import"          // import ... from "x"; import "x"; export ... from "x" (xem ReExport)
	ImportTypeOnly    ImportKind = "type_only"       // import type / export type ... from
	ImportDynamic     ImportKind = "dynamic"         // import("x")
	ImportRequire     ImportKind = "require"         // require("x"), import x = require("x")
	ImportResolve     ImportKind = "require_resolve" // require.resolve("x")
	ImportMock        ImportKind = "mock"            // jest.mock / vi.mock / jest.requireActual ... ("x")
	ImportGlob        ImportKind = "glob"            // import.meta.glob("pat"), require.context(dir, ...)
	ImportURL         ImportKind = "url"             // new URL("x", import.meta.url)
	ImportReExport    ImportKind = "reexport"        // export * from / export {a} from
	ImportTripleSlash ImportKind = "triple_slash"    // /// <reference path="x" />
)

// ImportRef là một tham chiếu tìm thấy trong văn bản nguồn.
//
// Dynamic=true: đối số không phải chuỗi hằng. Khi đó Spec là phần tĩnh đầu (có thể rỗng) của template/chuỗi nối,
// ví dụ `./locales/${x}.json` → Spec="./locales/", Suffix=".json". Phần tĩnh rỗng → nới.
type ImportRef struct {
	Spec    string     `json:"spec"`
	Suffix  string     `json:"suffix,omitempty"`
	Kind    ImportKind `json:"kind"`
	Line    int        `json:"line"`
	Dynamic bool       `json:"dynamic,omitempty"`
}

// ScanResult là đầu ra của bộ quét cho một tệp. Uncertain=true: không tin được danh sách import (cả tệp nới theo dự án).
type ScanResult struct {
	Imports         []ImportRef `json:"imports"`
	Uncertain       bool        `json:"uncertain,omitempty"`
	UncertainReason string      `json:"uncertain_reason,omitempty"`
}

// EdgeKind là loại cạnh trong đồ thị mức tệp.
type EdgeKind string

const (
	EdgeImport      EdgeKind = "import"
	EdgeTypeOnly    EdgeKind = "type_only"
	EdgeReExport    EdgeKind = "reexport"
	EdgeMod         EdgeKind = "mod"
	EdgeInclude     EdgeKind = "include"
	EdgeProjectRef  EdgeKind = "project_ref"
	EdgeConftest    EdgeKind = "conftest"
	EdgeImplicit    EdgeKind = "implicit"
	EdgeFixtureData EdgeKind = "fixture_data"
	EdgePattern     EdgeKind = "pattern"
	EdgeProbe       EdgeKind = "probe"
	EdgeGenerated   EdgeKind = "generated"
	EdgeRule        EdgeKind = "rule"
	EdgeMock        EdgeKind = "mock"
)

// Provenance là nguồn của một cạnh.
type Provenance string

const (
	ProvScan     Provenance = "scan"
	ProvRule     Provenance = "rule"
	ProvProvider Provenance = "provider"
	ProvDyn      Provenance = "dyn"
)

// Edge: From phụ thuộc vào To (đổi To thì From bị ảnh hưởng). Với Kind=EdgePattern, To là glob đánh giá trên cả hai cây.
type Edge struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	Kind       EdgeKind   `json:"kind"`
	Line       int        `json:"line,omitempty"`
	Spec       string     `json:"spec,omitempty"`
	Provenance Provenance `json:"provenance,omitempty"`
}

// UnresolvedClass phân loại import chưa phân giải.
type UnresolvedClass string

const (
	UnresolvedUnknown       UnresolvedClass = "unknown"
	UnresolvedDynamic       UnresolvedClass = "dynamic"
	UnresolvedAmbiguous     UnresolvedClass = "ambiguous"
	UnresolvedUncertainScan UnresolvedClass = "uncertain_scan"
)

// Unresolved: import của File không phân giải được. Khi một tệp trong ScopeDir (cùng dự án/gói) đổi, phần chọn nới tới
// ScopeTier/ScopeDir. ScopeDir rỗng với ScopeTier=full nghĩa là cả repo.
type Unresolved struct {
	File      string          `json:"file"`
	Spec      string          `json:"spec"`
	Class     UnresolvedClass `json:"class"`
	Line      int             `json:"line,omitempty"`
	ScopeTier Tier            `json:"scope_tier"`
	ScopeDir  string          `json:"scope_dir"`
	Detail    string          `json:"detail,omitempty"`
}

// TestUnit là đơn vị test runner chạy riêng được (mục 2.3).
type TestUnit struct {
	Eco     string   `json:"eco"`
	ID      string   `json:"id"`      // định danh ổn định: tệp test (JS/Python), "crate::target"...
	Project string   `json:"project"` // thư mục dự án chứa đơn vị
	Files   []string `json:"files"`
	Runner  string   `json:"runner,omitempty"` // jest, vitest, node-test, pytest, cargo...
	// Whole: đơn vị "cả bộ test của dự án" khi không biết tập tệp test (mẫu test do code tính). Files rỗng; runner chạy
	// cả dự án, không kèm danh sách.
	Whole bool `json:"whole,omitempty"`
}

// SelectedTest là một đơn vị test được chọn kèm chuỗi lý do.
type SelectedTest struct {
	Unit    string   `json:"unit"`
	Project string   `json:"project,omitempty"`
	Runner  string   `json:"runner,omitempty"`
	Files   []string `json:"files"`
	Reasons []string `json:"reasons"`
	Whole   bool     `json:"whole,omitempty"` // chạy cả bộ test của dự án (xem TestUnit.Whole)
}

// Selection là kết quả chọn (tương thích về ý nghĩa với bint/internal/affected.Selection).
type Selection struct {
	Scope      Tier           `json:"scope"`
	Base       string         `json:"base,omitempty"`
	Head       string         `json:"head,omitempty"`
	Changed    []string       `json:"changed"`
	Tests      []SelectedTest `json:"tests,omitempty"`
	Widening   []Widening     `json:"widening,omitempty"`
	Warnings   []Widening     `json:"warnings,omitempty"` // không đổi phạm vi (case_mismatch, rule_rejected...)
	AlwaysRun  []string       `json:"always_run,omitempty"`
	IndexStats map[string]int `json:"index_stats,omitempty"`
}
