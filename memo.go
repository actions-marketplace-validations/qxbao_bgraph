package bgraph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/gitx"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

// memo keeps commit trees and their analysis results between Select calls (commit trees are immutable by OID). The working tree
// is not memoized: it changes between calls; its cost is reduced by the content-keyed scan cache.
type memo struct {
	mu       sync.Mutex
	trees    map[string]*gitx.CommitTree // commit OID → tree
	oids     map[tree.Tree]string        // tree → commit OID
	analyses map[memoKey]*eco.Analysis
	dir      string // durable analysis snapshot directory; empty → memory only
}

type memoKey struct {
	t     tree.Tree
	eco   string
	rules string // contents of bgraph.toml used for analysis (the [[generated]] rules change the result)
}

const memoMaxTrees = 8

func (m *memo) commitTree(ctx context.Context, r *gitx.Repo, oid string) (*gitx.CommitTree, error) {
	m.mu.Lock()
	if t, ok := m.trees[oid]; ok {
		m.mu.Unlock()
		return t, nil
	}
	m.mu.Unlock()
	t, err := r.CommitTree(ctx, oid)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trees == nil {
		m.trees = map[string]*gitx.CommitTree{}
	}
	if len(m.trees) >= memoMaxTrees {
		// Drop everything when full (simple, deterministic); along with the analyses of those trees.
		m.trees = map[string]*gitx.CommitTree{}
		m.oids = nil
		m.analyses = nil
	}
	m.trees[oid] = t
	if m.oids == nil {
		m.oids = map[tree.Tree]string{}
	}
	m.oids[t] = oid
	return t, nil
}

// memoEco wraps an ecosystem: the memoized Analyze of a commit tree is reused.
type memoEco struct {
	eco.Ecosystem
	m     *memo
	rules string
}

func (e memoEco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	if _, ok := t.(*gitx.CommitTree); !ok {
		return e.Ecosystem.Analyze(ctx, t, sc)
	}
	k := memoKey{t, e.Name(), e.rules}
	e.m.mu.Lock()
	if a, ok := e.m.analyses[k]; ok {
		e.m.mu.Unlock()
		return a, nil
	}
	oid := e.m.oids[t]
	e.m.mu.Unlock()
	file := e.m.snapshotPath(e.Name(), e.Version(), oid, e.rules)
	a := loadAnalysis(file)
	if a == nil {
		var err error
		a, err = e.Ecosystem.Analyze(ctx, t, sc)
		if err != nil {
			return nil, err
		}
		saveAnalysis(file, a)
	}
	e.m.mu.Lock()
	if e.m.analyses == nil {
		e.m.analyses = map[memoKey]*eco.Analysis{}
	}
	e.m.analyses[k] = a
	e.m.mu.Unlock()
	return a, nil
}

func (e memoEco) ClassifyChange(p string, base, head tree.Tree, a *eco.Analysis) ([]model.Widening, bool) {
	return e.Ecosystem.ClassifyChange(p, base, head, a)
}

// snapshotPath: the analysis snapshot file for (ecosystem, version, scanner version, link semantics of trees, commit, bgraph.toml).
func (m *memo) snapshotPath(ecoName, ver, oid, rules string) string {
	if m.dir == "" || oid == "" {
		return ""
	}
	h := sha256.Sum256([]byte(ecoName + "|" + ver + "|" + scancache.ScannerVersion + "|" + tree.LinkSemantics + "|" + oid + "|" + rules))
	return filepath.Join(m.dir, "analysis", ecoName+"-"+hex.EncodeToString(h[:16])+".gob")
}

// snapshotVersion changes when the eco.Analysis format changes.
const snapshotVersion = 1

type snapshot struct {
	Version  int
	Analysis eco.Analysis
}

// loadAnalysis loads a snapshot; any error → nil (re-analyze). A snapshot is only a speedup, never the sole source of truth:
// the key covers every input of the analysis (immutable commit, rules, analyzer and scanner versions).
func loadAnalysis(file string) *eco.Analysis {
	if file == "" {
		return nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var s snapshot
	if gob.NewDecoder(bytes.NewReader(b)).Decode(&s) != nil || s.Version != snapshotVersion {
		return nil
	}
	if s.Analysis.Roles == nil {
		s.Analysis.Roles = map[string]model.Role{}
	}
	return &s.Analysis
}

const maxSnapshots = 32

// saveAnalysis writes to a temp file then renames (retrying on Windows when antivirus holds the handle); errors are ignored.
func saveAnalysis(file string, a *eco.Analysis) {
	if file == "" {
		return
	}
	dir := filepath.Dir(file)
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	var buf bytes.Buffer
	if gob.NewEncoder(&buf).Encode(snapshot{Version: snapshotVersion, Analysis: *a}) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "tmp-*.gob")
	if err != nil {
		return
	}
	_, werr := tmp.Write(buf.Bytes())
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return
	}
	for i := 0; i < 5; i++ {
		if err = os.Rename(tmp.Name(), file); err == nil {
			break
		}
		time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return
	}
	pruneSnapshots(dir)
}

// pruneSnapshots keeps the newest maxSnapshots files.
func pruneSnapshots(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fi struct {
		name string
		t    time.Time
	}
	var fs []fi
	for _, e := range ents {
		if info, err := e.Info(); err == nil && !e.IsDir() && filepath.Ext(e.Name()) == ".gob" {
			fs = append(fs, fi{e.Name(), info.ModTime()})
		}
	}
	if len(fs) <= maxSnapshots {
		return
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].t.After(fs[j].t) })
	for _, f := range fs[maxSnapshots:] {
		os.Remove(filepath.Join(dir, f.name))
	}
}
