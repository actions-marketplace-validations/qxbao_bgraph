// Package e2e runs the trap fixtures (docs/plan.md §10.1): each testdata/fixtures/**/*.txtar file describes a base tree, a change and hand-written expected answers.
//
// Format (txtar style):
//
//	free-form comment at the top
//	-- base/<path> --        file in the base tree (also in the current tree unless overridden/deleted)
//	-- head/<path> --        file added or modified in the current tree
//	-- delete/<path> --      file deleted in the current tree (content ignored)
//	-- want --               expected answers, one assertion per line:
//	    select <unit>        must be selected (or the scope is full)
//	    noselect <unit>      must not be selected (and the scope is not full)
//	    scope <tier>         scope must equal this
//	    widen <reason>       there must be a widening with this reason
//	    nowiden <reason>     there must be no widening with this reason
//	    full                 scope must be full
//	    notfull              scope must not be full
//	    warn <reason>        there must be a warning with this reason
//
// A "# ..." line in want is a comment.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/node"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

type fixture struct {
	base, head map[string]string
	deleted    []string
	want       []string
}

func parseFixture(data string) (fixture, error) {
	f := fixture{base: map[string]string{}, head: map[string]string{}}
	data = strings.ReplaceAll(data, "\r\n", "\n")
	lines := strings.SplitAfter(data, "\n")
	var cur string
	var buf strings.Builder
	flush := func() {
		if cur == "" {
			return
		}
		content := buf.String()
		switch {
		case cur == "want":
			for _, l := range strings.Split(content, "\n") {
				l = strings.TrimSpace(l)
				if l != "" && !strings.HasPrefix(l, "#") {
					f.want = append(f.want, l)
				}
			}
		case strings.HasPrefix(cur, "base/"):
			f.base[strings.TrimPrefix(cur, "base/")] = content
		case strings.HasPrefix(cur, "head/"):
			f.head[strings.TrimPrefix(cur, "head/")] = content
		case strings.HasPrefix(cur, "delete/"):
			f.deleted = append(f.deleted, strings.TrimPrefix(cur, "delete/"))
		}
		buf.Reset()
	}
	for _, l := range lines {
		t := strings.TrimRight(l, "\n")
		if strings.HasPrefix(t, "-- ") && strings.HasSuffix(t, " --") && len(t) > 6 {
			flush()
			cur = strings.TrimSpace(t[3 : len(t)-3])
			continue
		}
		if cur != "" {
			buf.WriteString(l)
		}
	}
	flush()
	return f, nil
}

// run builds the two trees, computes the change set and calls the selector with every supported ecosystem.
func run(t *testing.T, f fixture) model.Selection {
	t.Helper()
	headFiles := map[string]string{}
	for p, c := range f.base {
		headFiles[p] = c
	}
	for p, c := range f.head {
		headFiles[p] = c
	}
	for _, p := range f.deleted {
		delete(headFiles, p)
	}
	baseT := tree.NewMapTree(f.base)
	headT := tree.NewMapTree(headFiles)
	var changed []selector.Changed
	for p, c := range headFiles {
		old, ok := f.base[p]
		switch {
		case !ok:
			changed = append(changed, selector.Changed{Path: p, Status: 'A', NewMode: "100644"})
		case old != c:
			changed = append(changed, selector.Changed{Path: p, Status: 'M', OldMode: "100644", NewMode: "100644"})
		}
	}
	for _, p := range f.deleted {
		changed = append(changed, selector.Changed{Path: p, Status: 'D', OldMode: "100644"})
	}
	var rf *rules.File
	var rerr error
	if c, ok := headFiles["bgraph.toml"]; ok {
		rf, rerr = rules.Parse("bgraph.toml", []byte(c))
	}
	return selector.Select(context.Background(), selector.Input{
		Base: "base", Head: "head", Changed: changed, BaseTree: baseT, HeadTree: headT,
		Ecos: []eco.Ecosystem{node.New(rf)}, Scanner: scancache.New(), Rules: rf, RulesErr: rerr,
	})
}

func check(t *testing.T, sel model.Selection, want []string) {
	t.Helper()
	units := map[string]bool{}
	for _, st := range sel.Tests {
		units[st.Unit] = true
	}
	hasReason := func(ws []model.Widening, r string) bool {
		return slices.ContainsFunc(ws, func(w model.Widening) bool { return w.Reason == r })
	}
	isFull := sel.Scope == model.TierFull
	for _, w := range want {
		verb, arg, _ := strings.Cut(w, " ")
		arg = strings.TrimSpace(arg)
		ok := true
		switch verb {
		case "select":
			ok = isFull || units[arg]
		case "noselect":
			ok = !isFull && !units[arg]
		case "scope":
			ok = string(sel.Scope) == arg
		case "widen":
			ok = hasReason(sel.Widening, arg)
		case "nowiden":
			ok = !hasReason(sel.Widening, arg)
		case "full":
			ok = isFull
		case "notfull":
			ok = !isFull
		case "warn":
			ok = hasReason(sel.Warnings, arg)
		default:
			t.Fatalf("unknown assertion: %q", w)
		}
		if !ok {
			t.Errorf("failed: %s", w)
		}
	}
	if t.Failed() {
		b, _ := json.MarshalIndent(sel, "", "  ")
		t.Logf("selection:\n%s", b)
	}
}

func TestFixtures(t *testing.T) {
	var paths []string
	err := filepath.WalkDir("testdata/fixtures", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".txtar") {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no fixtures")
	}
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.ToSlash(strings.TrimPrefix(p, filepath.FromSlash("testdata/fixtures/"))), ".txtar")
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parseFixture(string(b))
			if err != nil {
				t.Fatal(err)
			}
			if len(f.want) == 0 {
				t.Fatal("fixture is missing a want section")
			}
			check(t, run(t, f), f.want)
		})
	}
}
