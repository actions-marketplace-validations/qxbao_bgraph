package evalkit

import (
	"encoding/json"
	"os"
)

// rubyKinds gives the manifest kind of every repo of tasks-ruby.jsonl; unknown repos default to "library". Fixed before looking at
// any result: applications ship a command or service (CLIs, site generators, log collectors, packagers), libraries are required by
// other programs.
var rubyKinds = map[string]string{
	"asciidoctor/asciidoctor": "library", "faker-ruby/faker": "library", "fastlane/fastlane": "application",
	"fluent/fluentd": "application", "jekyll/jekyll": "application", "jordansissel/fpm": "application", "rubocop/rubocop": "application",
}

// RubyRepoKind is the manifest kind of a Ruby repo.
func RubyRepoKind(repo string) string { return kindOf(rubyKinds, repo) }

// BuildRubyManifest builds the deterministic Ruby manifest (BuildCountManifest split rule, kinds from rubyKinds). metaPath (optional)
// supplies the dataset revisions recorded by fetch_ruby.py (meta key ruby_datasets).
func BuildRubyManifest(t1 map[string]*ManifestRepo, metaPath string, capPer int) Manifest {
	ds := []DatasetInfo{
		{Name: "SWE-bench/SWE-bench_Multilingual", URL: "https://huggingface.co/datasets/SWE-bench/SWE-bench_Multilingual", Notes: "test split; keeps Ruby repos (instances whose test patch touches a .rb file)"},
		{Name: "ByteDance-Seed/Multi-SWE-RL", URL: "https://huggingface.co/datasets/ByteDance-Seed/Multi-SWE-RL", Notes: "ruby/ directories (JSONL: asciidoctor, jekyll); FAIL_TO_PASS = f2p_tests keys, else n2p_tests keys (task field f2p_kind); a training set, not harness-curated like Multilingual"},
	}
	if b, err := os.ReadFile(metaPath); err == nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(b, &m) == nil {
			var revs map[string]string
			if json.Unmarshal(m["ruby_datasets"], &revs) == nil {
				for i := range ds {
					ds[i].Revision = revs[ds[i].Name]
				}
			}
		}
	}
	for _, r := range t1 {
		if r.Kind == "" {
			r.Kind = RubyRepoKind(r.Name)
		}
	}
	return BuildCountManifest("ruby", t1, ds, capPer)
}
