package evalkit

import "testing"

func TestSkipAndKeptClass(t *testing.T) {
	for in, want := range map[string]string{
		"python: pkg/new.py: status A (only modifications of existing files are observable: ...)": "file added",
		"python: docs/x.rst: not a python source file the collector observes":                     "non-.py file changed",
		"python: setup.py: role manifest/manifest at base/head is not source or test":             "changed .py is not source/test",
		"python: no records (run `bgraph record`)":                                                "no records",
		"python: widening global_input on .github/x is global or full":                            "global/full widening",
		"python: environment fingerprint: exec: not found":                                        "interpreter",
		"": "",
	} {
		if got := SkipClass(in); got != want {
			t.Errorf("SkipClass(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"kept by the dynamic layer: recorded dependencies include the changed file a.py":     "deps include a changed file",
		"kept by the dynamic layer: record not usable: stale: dependency a.py differs":       "stale record",
		"kept by the dynamic layer: record not usable: incomplete record: subprocess":        "incomplete record",
		"kept by the dynamic layer: record not usable: the recorded run did not pass":        "failed record",
		"kept by the dynamic layer: no record for the current environment (2 for other ...)": "other environment",
		"kept by the dynamic layer: no record":                                               "no record",
	} {
		if got := keptClass(in); got != want {
			t.Errorf("keptClass(%q) = %q, want %q", in, got, want)
		}
	}
}
