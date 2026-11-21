package scancache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// countTree đếm số lần Read để chứng minh lần chạy ấm không đọc tệp.
type countTree struct {
	tree.Tree
	reads atomic.Int64
}

func (c *countTree) Read(p string) ([]byte, error) {
	c.reads.Add(1)
	return c.Tree.Read(p)
}

func synthFile(i int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "// tệp %d\nimport React from 'react';\nimport type { T } from './types%d';\n", i, i%7)
	for j := 0; j < 12; j++ {
		fmt.Fprintf(&sb, "import { f%d } from './mod/m%d_%d';\n", j, i, j)
	}
	fmt.Fprintf(&sb, "const x = require('./cjs%d');\nconst y = await import(`./dyn/${name}.json`);\nexport * from './re%d';\n", i%13, i)
	for j := 0; j < 40; j++ {
		fmt.Fprintf(&sb, "export function g%d_%d(a: number) { return a + %d }\n", i, j, j)
	}
	return sb.String()
}

func synthTree(n int) (*tree.MapTree, []string) {
	m := map[string]string{}
	var paths []string
	for i := 0; i < n; i++ {
		ext := []string{".ts", ".tsx", ".js", ".mjs"}[i%4]
		p := fmt.Sprintf("src/d%d/f%d%s", i%50, i, ext)
		m[p] = synthFile(i)
		paths = append(paths, p)
	}
	m["README.md"] = "# readme"
	return tree.NewMapTree(m), append(paths, "README.md")
}

// norm: nil và rỗng coi như nhau khi so sánh.
func norm(r model.ScanResult) model.ScanResult {
	if len(r.Imports) == 0 {
		r.Imports = nil
	}
	return r
}

func TestCodecRoundTrip(t *testing.T) {
	r := model.ScanResult{Uncertain: true, UncertainReason: "vì sao", Imports: []model.ImportRef{
		{Spec: "./a", Kind: model.ImportStatic, Line: 3},
		{Spec: "./l/", Suffix: ".json", Kind: model.ImportDynamic, Line: 99999, Dynamic: true},
		{Spec: "x", Kind: "kind_lạ", Line: 1},
	}}
	got, err := decode(encode(r))
	if err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("got %+v err %v", got, err)
	}
	if _, err := decode([]byte{1, 0}); err == nil {
		t.Fatal("cần lỗi với dữ liệu cụt")
	}
	if _, err := decode(nil); err == nil {
		t.Fatal("cần lỗi với dữ liệu rỗng")
	}
}

func TestRoundTripPersistent(t *testing.T) {
	dir := t.TempDir()
	tr, paths := synthTree(40)
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]model.ScanResult{}
	for _, p := range paths {
		if lang(p) == "" {
			continue
		}
		r, err := c.Scan(tr, p)
		if err != nil {
			t.Fatal(err)
		}
		want[p] = r
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Fatalf("cảnh báo: %v", w)
	}

	c2, _ := Open(dir)
	defer c2.Close()
	ct := &countTree{Tree: tr}
	for p, w := range want {
		r, err := c2.Scan(ct, p)
		if err != nil || !reflect.DeepEqual(norm(r), norm(w)) {
			t.Fatalf("%s: lệch kết quả (err %v)", p, err)
		}
	}
	if ct.reads.Load() != 0 || c2.Misses != 0 || c2.Hits != len(want) {
		t.Fatalf("lần ấm phải không đọc tệp: reads=%d misses=%d hits=%d", ct.reads.Load(), c2.Misses, c2.Hits)
	}
}

func TestVersionBumpInvalidates(t *testing.T) {
	dir := t.TempDir()
	tr, paths := synthTree(8)
	c, _ := Open(dir)
	_ = c.Prefetch(context.Background(), tr, paths, 4)
	c.Close()

	// Giả lập bộ quét phiên bản cũ bằng cách sửa meta.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, dbName)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET v='js-0' WHERE k='scanner_version'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	c2, _ := Open(dir)
	ct := &countTree{Tree: tr}
	if _, err := c2.Scan(ct, paths[0]); err != nil {
		t.Fatal(err)
	}
	if ct.reads.Load() != 1 {
		t.Fatal("lệch phiên bản phải vô hiệu cache")
	}
	var n int
	if err := c2.db.QueryRow(`SELECT count(*) FROM scan`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bảng scan phải được dựng lại rỗng: n=%d err=%v", n, err)
	}
	var v string
	_ = c2.db.QueryRow(`SELECT v FROM meta WHERE k='scanner_version'`).Scan(&v)
	if v != ScannerVersion {
		t.Fatalf("meta chưa cập nhật: %q", v)
	}
	c2.Close()
}

func TestConcurrentScan(t *testing.T) {
	dir := t.TempDir()
	tr, paths := synthTree(64)
	c, _ := Open(dir)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range paths {
				p := paths[(i+g)%len(paths)]
				if lang(p) == "" {
					continue
				}
				if _, err := c.Scan(tr, p); err != nil {
					t.Error(err)
				}
			}
			if g%8 == 0 {
				_ = c.Flush()
			}
		}(g)
	}
	wg.Wait()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Fatalf("cảnh báo: %v", w)
	}
	c2, _ := Open(dir)
	defer c2.Close()
	ct := &countTree{Tree: tr}
	for _, p := range paths {
		if lang(p) != "" {
			if _, err := c2.Scan(ct, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ct.reads.Load() != 0 {
		t.Fatalf("đọc %d tệp dù đã cache", ct.reads.Load())
	}
}

func TestTwoInstancesSameFile(t *testing.T) {
	dir := t.TempDir()
	trA, pa := synthTree(30)
	m := map[string]string{}
	var pb []string
	for i := 100; i < 130; i++ {
		p := fmt.Sprintf("b/f%d.ts", i)
		m[p] = synthFile(i)
		pb = append(pb, p)
	}
	trB := tree.NewMapTree(m)

	a, _ := Open(dir)
	b, _ := Open(dir) // "tiến trình thứ hai"
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = a.Prefetch(context.Background(), trA, pa, 4); _ = a.Flush() }()
	go func() { defer wg.Done(); _ = b.Prefetch(context.Background(), trB, pb, 4); _ = b.Flush() }()
	wg.Wait()
	a.Close()
	b.Close()
	if w := append(a.Warnings(), b.Warnings()...); len(w) != 0 {
		t.Fatalf("cảnh báo: %v", w)
	}
	c, _ := Open(dir)
	defer c.Close()
	ct := &countTree{Tree: trA}
	for _, p := range pa {
		if lang(p) != "" {
			_, _ = c.Scan(ct, p)
		}
	}
	cb := &countTree{Tree: trB}
	for _, p := range pb {
		_, _ = c.Scan(cb, p)
	}
	if ct.reads.Load()+cb.reads.Load() != 0 {
		t.Fatalf("kết quả của cả hai tiến trình phải có trong DB: %d + %d lần đọc", ct.reads.Load(), cb.reads.Load())
	}
}

func TestCorruptRecovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dbName), []byte(strings.Repeat("không phải sqlite ", 500)), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, paths := synthTree(5)
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.db == nil {
		t.Fatalf("phải tạo lại kho; cảnh báo: %v", c.Warnings())
	}
	if len(c.Warnings()) == 0 {
		t.Fatal("phải có cảnh báo về tệp hỏng")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, dbName+".corrupt-*")); len(m) != 1 {
		t.Fatalf("tệp hỏng phải được đổi tên: %v", m)
	}
	if _, err := c.Scan(tr, paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, _ := Open(dir)
	defer c2.Close()
	ct := &countTree{Tree: tr}
	_, _ = c2.Scan(ct, paths[0])
	if ct.reads.Load() != 0 {
		t.Fatal("kho tạo lại phải bền")
	}
}

func TestOpenUnusableDirFallsBack(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tệp")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	c, err := Open(filepath.Join(f, "con")) // dir nằm dưới một tệp thường
	if err != nil {
		t.Fatal(err)
	}
	tr, paths := synthTree(2)
	if _, err := c.Scan(tr, paths[0]); err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings()) == 0 {
		t.Fatal("phải có cảnh báo")
	}
	c.Close()
}

func TestPrefetchMatchesSequential(t *testing.T) {
	tr, paths := synthTree(200)
	seq := New()
	par, _ := Open(t.TempDir())
	if err := par.Prefetch(context.Background(), tr, paths, 8); err != nil {
		t.Fatal(err)
	}
	if par.Misses != 200 {
		t.Fatalf("misses=%d, muốn 200 (README bị bỏ qua)", par.Misses)
	}
	ct := &countTree{Tree: tr}
	for _, p := range paths {
		if lang(p) == "" {
			if _, err := par.Scan(ct, p); err == nil {
				t.Fatal("tệp không có bộ quét phải lỗi")
			}
			continue
		}
		w, err := seq.Scan(tr, p)
		if err != nil {
			t.Fatal(err)
		}
		g, err := par.Scan(ct, p)
		if err != nil || !reflect.DeepEqual(g, w) {
			t.Fatalf("%s lệch", p)
		}
	}
	if ct.reads.Load() != 0 {
		t.Fatal("sau Prefetch, Scan không được đọc tệp")
	}
	par.Close()
	// Đường dẫn không tồn tại: không nghiêm trọng.
	if err := New().Prefetch(context.Background(), tr, []string{"khong/co.ts"}, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New().Prefetch(ctx, tr, paths, 4); err == nil {
		t.Fatal("ctx huỷ phải trả lỗi")
	}
}

func TestDefaultDir(t *testing.T) {
	a, err := DefaultDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := DefaultDir(t.TempDir())
	if a == b || len(filepath.Base(a)) != 16 {
		t.Fatalf("a=%s b=%s", a, b)
	}
}

// ---- Đo đạc ----

func benchResults(n int) (map[string]model.ScanResult, []string) {
	m := map[string]model.ScanResult{}
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		var r model.ScanResult
		for j := 0; j < 15; j++ {
			r.Imports = append(r.Imports, model.ImportRef{Spec: fmt.Sprintf("./mod/m%d_%d", i, j), Kind: model.ImportStatic, Line: j + 2})
		}
		k := fmt.Sprintf("%s:js:.ts:%040x", ScannerVersion, i)
		m[k] = r
		keys[i] = k
	}
	return m, keys
}

func BenchmarkCodec(b *testing.B) {
	m, _ := benchResults(1)
	var r model.ScanResult
	for _, v := range m {
		r = v
	}
	jb, _ := json.Marshal(r)
	bb := encode(r)
	b.Logf("kích thước: nhị phân %d B, JSON %d B", len(bb), len(jb))
	b.Run("bin-enc", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = encode(r)
		}
	})
	b.Run("json-enc", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = json.Marshal(r)
		}
	})
	b.Run("bin-dec", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = decode(bb)
		}
	})
	b.Run("json-dec", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var o model.ScanResult
			_ = json.Unmarshal(jb, &o)
		}
	})
}

// populate ghi n dòng tổng hợp vào kho trong dir.
func populate(b *testing.B, dir string, n int) []string {
	c, _ := Open(dir)
	rs, keys := benchResults(n)
	c.mu.Lock()
	for k, r := range rs {
		c.m[k] = r
		c.dirty[k] = struct{}{}
	}
	c.mu.Unlock()
	if err := c.Close(); err != nil || len(c.Warnings()) != 0 {
		b.Fatal(err, c.Warnings())
	}
	return keys
}

// BenchmarkLoadStrategy so nạp-hết-vào-map với tra từng khoá (mở mới + tra mọi khoá, như lần chạy ấm).
func BenchmarkLoadStrategy(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		dir := b.TempDir()
		keys := populate(b, dir, n)
		b.Run(fmt.Sprintf("bulk-%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c, _ := Open(dir)
				if got, err := c.loadAll(); err != nil || got != n {
					b.Fatal(got, err)
				}
				for _, k := range keys {
					if _, ok := c.lookup(k); !ok {
						b.Fatal("trượt")
					}
				}
				c.Close()
			}
		})
		b.Run(fmt.Sprintf("perkey-%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c, _ := Open(dir)
				for _, k := range keys {
					if _, ok := c.lookup(k); !ok {
						b.Fatal("trượt")
					}
				}
				c.Close()
			}
		})
		// Trường hợp thực tế: repo 2000 tệp trong kho 100k dòng (kho tích luỹ nhiều nhánh).
		if n == 100_000 {
			sub := keys[:2000]
			b.Run("bulk-100000-use2000", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					c, _ := Open(dir)
					_, _ = c.loadAll()
					for _, k := range sub {
						c.lookup(k)
					}
					c.Close()
				}
			})
			b.Run("perkey-100000-use2000", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					c, _ := Open(dir)
					for _, k := range sub {
						c.lookup(k)
					}
					c.Close()
				}
			})
		}
	}
}

func BenchmarkPrefetchCold(b *testing.B) {
	tr, paths := synthTree(10_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dir := b.TempDir()
		b.StartTimer()
		c, _ := Open(dir)
		if err := c.Prefetch(context.Background(), tr, paths, 16); err != nil {
			b.Fatal(err)
		}
		_ = c.Close()
	}
}

func BenchmarkPrefetchWarm(b *testing.B) {
	tr, paths := synthTree(10_000)
	dir := b.TempDir()
	c, _ := Open(dir)
	_ = c.Prefetch(context.Background(), tr, paths, 16)
	_ = c.Close()
	ct := &countTree{Tree: tr}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, _ := Open(dir) // thể hiện Cache mới: mọi khoá chỉ có trong DB
		if err := c.Prefetch(context.Background(), ct, paths, 16); err != nil {
			b.Fatal(err)
		}
		_ = c.Close()
	}
	if ct.reads.Load() != 0 {
		b.Fatalf("lần ấm đọc %d tệp", ct.reads.Load())
	}
}
