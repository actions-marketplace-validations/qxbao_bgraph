package evalkit

import (
	"encoding/json"
	"os"
)

// dartKinds gives the manifest kind of every repo of tasks-dart.jsonl; unknown repos default to "library". Fixed before looking at
// any result: applications ship a command, service or app (CLIs, code generators, servers, Flutter apps), frameworks host user code
// (state management, server and HDL frameworks, site generators, UI catalogs), libraries are imported by other programs.
var dartKinds = map[string]string{
	"flutter-bounty-hunters/static_shock": "framework", "verygoodopensource/dart_frog": "framework", "verygoodopensource/very_good_cli": "application",
	"atsign-foundation/noports": "application", "dart-lang/dartdoc": "application", "dart-lang/ffigen": "application",
	"dart-lang/pana": "application", "dart-lang/pub-dev": "application", "deveminsahin/starter_app": "application",
	"epam-cross-platform-lab/swagger-dart-code-generator": "application", "eseidel/space_gen": "application",
	"felangel/bloc": "framework", "firebase/firebase-functions-dart": "framework", "flutter/cocoon": "application",
	"flutter/genui": "framework", "intel/rohd": "framework", "nahpu/nahpu": "application", "rrousselgit/riverpod": "framework",
	"serverpod/serverpod": "framework", "serverpod/skills": "application", "tsutsu3/pi-hole-client": "application",
	"widgetbook/widgetbook": "framework",
}

// DartRepoKind is the manifest kind of a Dart repo.
func DartRepoKind(repo string) string { return kindOf(dartKinds, repo) }

// BuildDartManifest builds the deterministic Dart manifest (BuildCountManifest split rule, kinds from dartKinds). metaPath (optional)
// supplies the dataset revisions recorded by fetch_dart.py (meta key dart_datasets).
func BuildDartManifest(t1 map[string]*ManifestRepo, metaPath string, capPer int) Manifest {
	ds := []DatasetInfo{
		{Name: "nebius/Longtail-SWE", URL: "https://huggingface.co/datasets/nebius/Longtail-SWE", Notes: "hard and easy splits, language Dart"},
		{Name: "nebius/SWE-rebench-V2", URL: "https://huggingface.co/datasets/nebius/SWE-rebench-V2", Notes: "train split, language dart; FAIL_TO_PASS recorded by the harness test command (task field test_cmd)"},
	}
	if b, err := os.ReadFile(metaPath); err == nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(b, &m) == nil {
			var revs map[string]string
			if json.Unmarshal(m["dart_datasets"], &revs) == nil {
				for i := range ds {
					ds[i].Revision = revs[ds[i].Name]
				}
			}
		}
	}
	for _, r := range t1 {
		if r.Kind == "" {
			r.Kind = DartRepoKind(r.Name)
		}
	}
	return BuildCountManifest("dart", t1, ds, capPer)
}
