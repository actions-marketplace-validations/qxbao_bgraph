package rbdyn

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/tree"
)

// TestCollect runs the real collector (Ruby required, skipped otherwise) on units that exercise its observations: reads and
// listings, a Ruby child, a Ruby child that writes no coverage, a non-Ruby child, tests that touch Coverage, no tests.
func TestCollect(t *testing.T) {
	ruby := testRuby(t)
	files := map[string]string{
		"lib/lib.rb":            "module Lib\n  def self.used\n    1\n  end\n\n  def self.unused\n    2\n  end\nend\n",
		"data/x.txt":            "x\n",
		"data/sub/y.txt":        "y\n",
		"test/helper.rb":        "require \"minitest/autorun\"\nrequire \"lib\"\n",
		"test/read_test.rb":     "require \"helper\"\nclass ReadTest < Minitest::Test\n  def test_read\n    assert_equal \"x\\n\", File.read(\"data/x.txt\")\n    assert_equal 1, Dir.glob(\"data/sub/*.txt\").size\n    assert_equal 1, Lib.used\n  end\nend\n",
		"test/child_test.rb":    "require \"helper\"\nclass ChildTest < Minitest::Test\n  def test_child\n    assert_equal \"1\\n\", IO.popen([RbConfig.ruby, \"-e\", \"puts 1\"], &:read)\n    assert system(RbConfig.ruby, \"-e\", \"exit 0\")\n  end\nend\n",
		"test/exitbang_test.rb": "require \"helper\"\nclass ExitBangTest < Minitest::Test\n  def test_x\n    pid = spawn(RbConfig.ruby, \"-e\", \"exit!(0)\")\n    Process.wait(pid)\n    assert true\n  end\nend\n",
		"test/shell_test.rb":    "require \"helper\"\nclass ShellTest < Minitest::Test\n  def test_x\n    assert system(\"echo hi > \" + File::NULL)\n  end\nend\n",
		"test/cov_test.rb":      "require \"helper\"\nrequire \"coverage\"\nclass CovTest < Minitest::Test\n  def test_x\n    Coverage.peek_result\n    Coverage.result(stop: false, clear: true)\n    assert true\n  end\nend\n",
		"test/zero_test.rb":     "require \"helper\"\n",
	}
	root := t.TempDir()
	for rel, c := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "data", "untracked.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := tree.NewMapTree(files)
	var units []Unit
	for _, u := range []string{"read", "child", "exitbang", "shell", "cov", "zero"} {
		units = append(units, Unit{ID: "test/" + u + "_test.rb", Project: ".", Runner: "minitest", Config: "|lib,test"})
	}
	t.Setenv("RUBYOPT", "")
	t.Setenv("BUNDLE_GEMFILE", "")
	outs, err := Collect(context.Background(), RunOptions{Ruby: ruby, Root: root, TmpDir: t.TempDir(), Timeout: 2 * time.Minute, Jobs: 3}, units)
	if err != nil {
		t.Fatal(err)
	}
	recs := map[string]Outcome{}
	for _, o := range outs {
		recs[o.Unit] = o
	}
	rec := func(u string) (Outcome, string, bool, bool, []string) {
		o := recs["test/"+u+"_test.rb"]
		r := RecordOf(o, Unit{ID: "test/" + u + "_test.rb"}, tr, root, "env", "c", time.Now())
		return o, r.Incomplete, r.Complete, r.Pass, r.Reads
	}
	o, why, complete, pass, reads := rec("read")
	if !complete || !pass || !slices.Contains(reads, "data/x.txt") || !slices.Contains(reads, "data/sub/y.txt") {
		t.Fatalf("read: complete=%v pass=%v reads=%v (%s) tail %s", complete, pass, reads, why, o.Tail)
	}
	r := RecordOf(o, Unit{ID: "test/read_test.rb"}, tr, root, "env", "c", time.Now())
	if ms, _ := ParseMethods(r.Methods["lib/lib.rb"]); !slices.Equal(ms, []Method{{2, 4, 1}, {6, 8, 0}}) {
		t.Errorf("read: lib.rb table %v", ms)
	}
	if _, why, complete, pass, _ := rec("child"); !complete || !pass {
		t.Errorf("child: complete=%v pass=%v (%s)", complete, pass, why)
	}
	if _, why, complete, _, _ := rec("exitbang"); complete || !strings.Contains(why, "wrote no coverage") {
		t.Errorf("exitbang: complete=%v (%s)", complete, why)
	}
	if _, why, complete, _, _ := rec("shell"); complete || !strings.Contains(why, "non-Ruby process") {
		t.Errorf("shell: complete=%v (%s)", complete, why)
	}
	if _, why, complete, _, _ := rec("cov"); complete || !strings.Contains(why, "Coverage.result") {
		t.Errorf("cov: complete=%v (%s)", complete, why)
	}
	if _, why, complete, pass, _ := rec("zero"); complete || pass || !strings.Contains(why, "no tests ran") {
		t.Errorf("zero: complete=%v pass=%v (%s)", complete, pass, why)
	}
	// An untracked repository file read by a unit makes it incomplete.
	o.Reads = append(o.Reads, filepath.ToSlash(filepath.Join(root, "data", "untracked.txt")))
	if r := RecordOf(o, Unit{ID: "test/read_test.rb"}, tr, root, "env", "c", time.Now()); r.Complete || !strings.Contains(r.Incomplete, "reads untracked file data/untracked.txt") {
		t.Errorf("untracked: complete=%v (%s)", r.Complete, r.Incomplete)
	}
}
