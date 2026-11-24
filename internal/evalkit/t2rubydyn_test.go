package evalkit

import "testing"

func TestRbReasonClass(t *testing.T) {
	for in, want := range map[string]string{
		"lib/a.rb:62: code outside method bodies changed": "code outside method bodies changed",
		"lib/a.rb: base: line 131: end without a block":   "base: line N: end without a block",
		"lib/a.rb: mode changed 100644 -> 100755":         "mode changed N -> N",
	} {
		if got := RbReasonClass(in); got != want {
			t.Errorf("RbReasonClass(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"executed the changed method Shop::Cart#total (lib/shop/cart.rb:8)":                                         "executed a changed method",
		"loaded lib/a.rb without a method table (loaded before the collector started): the change cannot be judged": "changed method not instrumented",
		"not narrowed by the Ruby method layer: lib/a.rb: status A":                                                 "layer skipped",
		"read the changed file lib/a.rb as data":                                                                    "read a changed file",
	} {
		if got := rbKeptClass(in); got != want {
			t.Errorf("rbKeptClass(%q) = %q, want %q", in, got, want)
		}
	}
}
