// Package model holds the data types shared between bgraph's packages (docs/plan.md §4.2).
//
// Path convention: every path is relative to the repo root, uses forward slashes, is path.Clean'ed, has no leading "./"; the repo root is ".".
package model

// Tier is the scope of a selection, from narrow to wide.
type Tier string

const (
	TierNone    Tier = "none"    // no test is affected
	TierGraph   Tier = "graph"   // the selection is provable from the graph
	TierPackage Tier = "package" // widened to a directory / package / Cargo target
	TierModule  Tier = "module"  // widened to a project (package.json, crate, Maven module, .NET project)
	TierFull    Tier = "full"    // run everything
)

// Rank: the ordering used to take the widest tier.
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

// Max returns the wider tier.
func Max(a, b Tier) Tier {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// Widening reasons (stable, machine-readable codes). Kept compatible with bint/internal/affected, plus the reasons from docs/plan.md §7.1.
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
	ReasonUnknownFile     = "unknown_file" // file belongs to no ecosystem and nobody references it
	// code/data file of an ecosystem that nothing in the graph depends on (test fixture read via fs, script/bin
	// run by a test via child_process, entry point): nobody known to use it → widen to the project.
	ReasonUnreferenced    = "unreferenced_file"
	ReasonUnsupported     = "unsupported_language"
	ReasonGeneratedOutput = "generated_output" // import of an ignored build output
	ReasonFrameworkMagic  = "framework_magic"  // Next file routing, auto-import...
	ReasonSymlinkOutside  = "symlink_outside"
	ReasonSubmodule       = "submodule"
	ReasonInternalError   = "internal_error" // bgraph's own error: always widen, never return empty
	ReasonRuleRejected    = "rule_rejected"  // an affects="none" rule matched a file that is a graph node: ignored (warning only, no widening)
)

// Widening records one widening. Path is the file that caused it; Scope is the widened scope (package/project directory; empty when Tier=full).
type Widening struct {
	Tier    Tier   `json:"tier"`
	Reason  string `json:"reason"`
	Path    string `json:"path,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Suggest string `json:"suggest,omitempty"` // suggested bgraph.toml rule to narrow it
	// Eco restricts a module/package widening to the test units of one ecosystem (empty: every ecosystem). Used by ecosystems whose
	// scope directory may also hold other ecosystems' projects (a Cargo.lock at the repo root must not select Node tests).
	Eco string `json:"eco,omitempty"`
}

// Role is the role of a file.
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

// ImportKind is the syntactic form of an import reference found by a scanner.
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

// ImportRef is a reference found in the source text.
//
// Dynamic=true: the argument is not a constant string. Spec is then the static head (possibly empty) of the template/concatenation,
// e.g. `./locales/${x}.json` → Spec="./locales/", Suffix=".json". An empty static part → widen.
type ImportRef struct {
	Spec    string     `json:"spec"`
	Suffix  string     `json:"suffix,omitempty"`
	Kind    ImportKind `json:"kind"`
	Line    int        `json:"line"`
	Dynamic bool       `json:"dynamic,omitempty"`
}

// ScanResult is a scanner's output for one file. Uncertain=true: the import list cannot be trusted (the whole file widens to the project).
type ScanResult struct {
	Imports         []ImportRef `json:"imports"`
	Uncertain       bool        `json:"uncertain,omitempty"`
	UncertainReason string      `json:"uncertain_reason,omitempty"`
	// Rust is set by the Rust scanner only (.rs files); Imports is empty for them.
	Rust *RustScan `json:"rust,omitempty"`
}

// RustScan is what the Rust scanner extracts from one .rs file: the module declarations that decide which Cargo target
// owns a file, and the include macros that read other files.
type RustScan struct {
	Mods     []RustMod     `json:"mods,omitempty"`
	Includes []RustInclude `json:"includes,omitempty"`
}

// RustMod is a `mod name;` declaration (inline `mod name { ... }` blocks are not listed: only their names appear in Nest).
type RustMod struct {
	Name string   `json:"name"`
	Line int      `json:"line"`
	Nest []string `json:"nest,omitempty"` // names of the enclosing inline modules, outermost first
	Path string   `json:"path,omitempty"` // literal #[path = "..."] attribute
	// Unknown: non-empty when the file this declaration loads cannot be determined lexically (cfg_attr(path), declared inside a
	// macro, non-literal #[path]...). The value says why.
	Unknown string `json:"unknown,omitempty"`
}

// Kinds of RustInclude.
const (
	RustIncludeLiteral  = "literal"  // relative to the file containing the macro
	RustIncludeManifest = "manifest" // concat!(env!("CARGO_MANIFEST_DIR"), "...") : relative to the package directory
	RustIncludeOutDir   = "outdir"   // concat!(env!("OUT_DIR"), "...") : build script output, not in the tree
	RustIncludeDir      = "dir"      // automod::dir!("path"): modules generated from the files of a directory (relative to the package directory)
	RustIncludeDynamic  = "dynamic"  // anything else
)

// RustInclude is an include!/include_str!/include_bytes! invocation (or automod::dir!, Macro "automod::dir").
type RustInclude struct {
	Macro string `json:"macro"` // include, include_str, include_bytes
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
	Line  int    `json:"line"`
}

// EdgeKind is the kind of an edge in the file-level graph.
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

// Provenance is where an edge came from.
type Provenance string

const (
	ProvScan     Provenance = "scan"
	ProvRule     Provenance = "rule"
	ProvProvider Provenance = "provider"
	ProvDyn      Provenance = "dyn"
)

// Edge: From depends on To (changing To affects From). For Kind=EdgePattern, To is a glob evaluated on both trees.
type Edge struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	Kind       EdgeKind   `json:"kind"`
	Line       int        `json:"line,omitempty"`
	Spec       string     `json:"spec,omitempty"`
	Provenance Provenance `json:"provenance,omitempty"`
}

// UnresolvedClass classifies an unresolved import.
type UnresolvedClass string

const (
	UnresolvedUnknown       UnresolvedClass = "unknown"
	UnresolvedDynamic       UnresolvedClass = "dynamic"
	UnresolvedAmbiguous     UnresolvedClass = "ambiguous"
	UnresolvedUncertainScan UnresolvedClass = "uncertain_scan"
)

// Unresolved: an import of File that could not be resolved. When a file in ScopeDir (same project/package) changes, the selection widens to
// ScopeTier/ScopeDir. An empty ScopeDir with ScopeTier=full means the whole repo.
type Unresolved struct {
	File      string          `json:"file"`
	Spec      string          `json:"spec"`
	Class     UnresolvedClass `json:"class"`
	Line      int             `json:"line,omitempty"`
	ScopeTier Tier            `json:"scope_tier"`
	ScopeDir  string          `json:"scope_dir"`
	Detail    string          `json:"detail,omitempty"`
}

// TestUnit is a unit a test runner can run on its own (docs/plan.md §2.3).
type TestUnit struct {
	Eco     string   `json:"eco"`
	ID      string   `json:"id"`      // stable identifier: test file (JS/Python), "crate::target"...
	Project string   `json:"project"` // directory of the project containing the unit
	Files   []string `json:"files"`
	Runner  string   `json:"runner,omitempty"` // jest, vitest, node-test, pytest, cargo...
	// Whole: the "whole test suite of the project" unit, used when the set of test files is unknown (test pattern computed by code). Files is empty; the runner runs
	// the whole project, without a list.
	Whole bool `json:"whole,omitempty"`
	// Package is the package name of the unit for runners that address units by package (cargo -p).
	Package string `json:"package,omitempty"`
}

// SelectedTest is a selected test unit together with its reason chain.
type SelectedTest struct {
	Unit    string   `json:"unit"`
	Project string   `json:"project,omitempty"`
	Runner  string   `json:"runner,omitempty"`
	Files   []string `json:"files"`
	Reasons []string `json:"reasons"`
	Whole   bool     `json:"whole,omitempty"`   // run the project's whole test suite (see TestUnit.Whole)
	Package string   `json:"package,omitempty"` // see TestUnit.Package
}

// Selection is the selection result (semantically compatible with bint/internal/affected.Selection).
type Selection struct {
	Scope      Tier           `json:"scope"`
	Base       string         `json:"base,omitempty"`
	Head       string         `json:"head,omitempty"`
	Changed    []string       `json:"changed"`
	Tests      []SelectedTest `json:"tests,omitempty"`
	Widening   []Widening     `json:"widening,omitempty"`
	Warnings   []Widening     `json:"warnings,omitempty"` // do not change the scope (case_mismatch, rule_rejected...)
	AlwaysRun  []string       `json:"always_run,omitempty"`
	IndexStats map[string]int `json:"index_stats,omitempty"`
}
