package dart

import (
	"maps"
	"slices"

	"github.com/qxbao/bgraph/internal/model"
)

// runnerDeps are the packages the test runners load themselves: the transitive dependencies of package:test,
// test_descriptor and test_process (Dart 3.14.1), and of flutter_test and integration_test (Flutter 3.44.0), SDK packages
// excepted. Pub resolves them, like any dependency, to the copy a resolution provides, so when one of them is a package of
// the repository (dart-lang/core holds args, path and collection), a change to it changes `dart test` itself for every
// package that resolves the local copy. Recompute with `dart pub deps --json` when the runners gain dependencies.
var runnerDeps = map[string]bool{}

func init() {
	for _, n := range []string{
		"_fe_analyzer_shared", "analyzer", "args", "async", "boolean_selector", "characters", "cli_config", "cli_util", "clock",
		"collection", "convert", "coverage", "crypto", "fake_async", "ffi", "file", "frontend_server_client", "glob",
		"http_multi_server", "http_parser", "io", "leak_tracker", "leak_tracker_flutter_testing", "leak_tracker_testing",
		"logging", "matcher", "material_color_utilities", "meta", "mime", "node_preamble", "package_config", "path", "platform",
		"pool", "process", "pub_semver", "shelf", "shelf_packages_handler", "shelf_static", "shelf_web_socket",
		"source_map_stack_trace", "source_maps", "source_span", "stack_trace", "stream_channel", "string_scanner", "sync_http",
		"term_glyph", "test", "test_api", "test_core", "test_descriptor", "test_process", "typed_data", "vector_math",
		"vm_service", "watcher", "web", "web_socket", "web_socket_channel", "webdriver", "webkit_inspection_protocol", "yaml",
	} {
		runnerDeps[n] = true
	}
}

// runnerNode is the virtual node standing for the code of an in-repository package that the test runner itself loads.
func runnerNode(dir string) string { return "<dartrunner:" + dir + ">" }

// runnerPackages makes every test of a package that may resolve an in-repository runner dependency Q to its local copy
// (Q itself, the packages depending on it and every package sharing a resolution with one of them) depend on Q's lib/:
// a change there can break or alter the runner of those tests.
func (b *builder) runnerPackages() {
	for _, d := range slices.Sorted(maps.Keys(b.r.pkgs)) {
		q := b.r.pkgs[d]
		if !runnerDeps[q.name] {
			continue
		}
		node := runnerNode(d)
		b.edge(node, escapeGlob(joinDir(d, "lib"))+"/**", model.EdgePattern, "test runner dependency "+q.name, 0)
		users := map[string]bool{}
		for _, u := range b.dependentsOf(d) {
			users[u] = true
			root := b.r.resolutionRoot(u)
			users[root] = true
			for _, m := range b.r.allMembers(root) {
				users[m] = true
			}
		}
		for _, u := range slices.Sorted(maps.Keys(users)) {
			for _, f := range b.units[u] {
				b.edge(f, node, model.EdgeImplicit, "the test runner loads "+q.name+" from "+d, 0)
			}
		}
	}
}
