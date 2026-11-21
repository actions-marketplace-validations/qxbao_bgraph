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

	"bgraph/internal/eco"
	"bgraph/internal/gitx"
	"bgraph/internal/model"
	"bgraph/internal/scancache"
	"bgraph/internal/tree"
)

// memo giữ cây commit và kết quả phân tích của chúng giữa các lần Select (cây commit bất biến theo OID). Cây làm việc
// không được ghi nhớ: nó đổi giữa các lần gọi; chi phí của nó giảm nhờ cache quét theo nội dung.
type memo struct {
	mu       sync.Mutex
	trees    map[string]*gitx.CommitTree // OID commit → cây
	oids     map[tree.Tree]string        // cây → OID commit
	analyses map[memoKey]*eco.Analysis
	dir      string // thư mục snapshot phân tích bền; rỗng → chỉ bộ nhớ
}

type memoKey struct {
	t     tree.Tree
	eco   string
	rules string // nội dung bgraph.toml dùng khi phân tích (luật [[generated]] đổi kết quả)
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
		// Bỏ hết khi đầy (đơn giản, tất định); kèm phân tích của các cây đó.
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

// memoEco bọc một hệ sinh thái: Analyze của cây commit đã ghi nhớ được dùng lại.
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

// snapshotPath: tệp snapshot phân tích cho (hệ sinh thái, phiên bản, phiên bản bộ quét, commit, bgraph.toml).
func (m *memo) snapshotPath(ecoName, ver, oid, rules string) string {
	if m.dir == "" || oid == "" {
		return ""
	}
	h := sha256.Sum256([]byte(ecoName + "|" + ver + "|" + scancache.ScannerVersion + "|" + oid + "|" + rules))
	return filepath.Join(m.dir, "analysis", ecoName+"-"+hex.EncodeToString(h[:16])+".gob")
}

// snapshotVersion đổi khi định dạng eco.Analysis đổi.
const snapshotVersion = 1

type snapshot struct {
	Version  int
	Analysis eco.Analysis
}

// loadAnalysis nạp snapshot; mọi lỗi → nil (phân tích lại). Snapshot chỉ là tăng tốc, không bao giờ là nguồn sự thật
// duy nhất: khoá gồm mọi đầu vào của phép phân tích (commit bất biến, luật, phiên bản bộ phân tích và bộ quét).
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

// saveAnalysis ghi tạm rồi rename (thử lại trên Windows khi antivirus giữ handle); lỗi bị bỏ qua.
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

// pruneSnapshots giữ maxSnapshots tệp mới nhất.
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
