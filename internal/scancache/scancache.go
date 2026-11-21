// Package scancache quét import có cache theo khoá nội dung (mục 4.4: đơn vị cache không phụ thuộc commit).
//
// Cache giữ map trong bộ nhớ và, nếu mở bằng Open, một kho SQLite bền (<dir>/scan.sqlite) để lần chạy sau không phải
// đọc lại tệp không đổi. Kho chỉ là tăng tốc: mọi lỗi kho (khoá, hỏng, đĩa đầy) hạ về chỉ-bộ-nhớ kèm cảnh báo, không
// bao giờ làm hỏng một lần chọn.
package scancache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"bgraph/internal/model"
	jsscan "bgraph/internal/scan/js"
	sfcscan "bgraph/internal/scan/sfc"
	"bgraph/internal/tree"
)

// ScannerVersion: tăng chuỗi này mỗi khi đổi hành vi bộ quét (lexer, luật nhận diện import) để vô hiệu cache bền.
const ScannerVersion = "js-1+sfc-1"

// Cache là eco.Scanner có cache, an toàn khi dùng đồng thời.
type Cache struct {
	mu       sync.Mutex
	m        map[string]model.ScanResult
	dirty    map[string]struct{} // khoá trong m chưa ghi xuống kho
	db       *sql.DB             // nil: chỉ-bộ-nhớ
	get      *sql.Stmt
	warnings []string
	Hits     int
	Misses   int
}

// New tạo cache rỗng, chỉ trong bộ nhớ.
func New() *Cache { return &Cache{m: map[string]model.ScanResult{}, dirty: map[string]struct{}{}} }

// DefaultDir trả thư mục cache của repo: os.UserCacheDir()/bgraph/<16 hex đầu của sha256(đường dẫn tuyệt đối)>.
// Không bao giờ nằm trong repo.
func DefaultDir(repoRoot string) (string, error) {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(abs))
	dir := filepath.Join(base, "bgraph", hex.EncodeToString(h[:])[:16])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Warnings trả bản sao các cảnh báo về kho bền (đã hạ về bộ nhớ, DB hỏng được đổi tên...).
func (c *Cache) Warnings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.warnings...)
}

// warn ghi cảnh báo; gọi khi đang giữ c.mu (hoặc khi chưa chia sẻ c).
func (c *Cache) warn(format string, a ...any) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, a...))
}

// lang chọn bộ quét theo đuôi tệp.
func lang(p string) string {
	switch path.Ext(p) {
	case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts":
		return "js"
	case ".vue", ".svelte", ".astro", ".mdx":
		return "sfc"
	}
	return ""
}

// cacheKey: khoá gồm phiên bản bộ quét, ngôn ngữ, đuôi tệp (đuôi đổi cách lex: JSX trong .tsx/.ts) và khoá nội dung.
func cacheKey(l, p, key string) string {
	return ScannerVersion + ":" + l + ":" + strings.ToLower(path.Ext(p)) + ":" + key
}

// lookup tìm trong bộ nhớ rồi kho bền. Không đụng tới Hits/Misses.
func (c *Cache) lookup(ck string) (model.ScanResult, bool) {
	c.mu.Lock()
	if r, ok := c.m[ck]; ok {
		c.mu.Unlock()
		return r, true
	}
	stmt := c.get
	c.mu.Unlock()
	if stmt == nil {
		return model.ScanResult{}, false
	}
	var blob []byte
	err := stmt.QueryRow(ck).Scan(&blob)
	if err == nil {
		r, derr := decode(blob)
		c.mu.Lock()
		defer c.mu.Unlock()
		if derr != nil { // dòng hỏng: coi như trượt, Flush sẽ ghi đè
			c.warn("scancache: bỏ qua dòng cache hỏng: %v", derr)
			return model.ScanResult{}, false
		}
		c.m[ck] = r
		return r, true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		c.mu.Lock()
		c.disableDB(err)
		c.mu.Unlock()
	}
	return model.ScanResult{}, false
}

// Scan quét tệp p của cây t.
func (c *Cache) Scan(t tree.Tree, p string) (model.ScanResult, error) {
	l := lang(p)
	if l == "" {
		return model.ScanResult{}, fmt.Errorf("không có bộ quét cho %s", p)
	}
	key := t.Key(p)
	ck := ""
	if key != "" {
		ck = cacheKey(l, p, key)
		if r, ok := c.lookup(ck); ok {
			c.mu.Lock()
			c.Hits++
			c.mu.Unlock()
			return r, nil
		}
	}
	b, err := t.Read(p)
	if err != nil {
		return model.ScanResult{}, err
	}
	var r model.ScanResult
	if l == "sfc" {
		r = sfcscan.Scan(p, b)
	} else {
		r = jsscan.Scan(p, b)
	}
	c.mu.Lock()
	c.Misses++
	if ck != "" {
		c.m[ck] = r
		c.dirty[ck] = struct{}{}
	}
	c.mu.Unlock()
	return r, nil
}

// Prefetch quét song song (workers goroutine, mặc định 16) mọi tệp trong paths mà khoá chưa có trong cache (bộ nhớ hay
// kho), để lần Scan sau đều trúng. Tệp không có bộ quét bị bỏ qua; lỗi đọc tệp không nghiêm trọng (Scan sẽ trả lại sau).
// Chỉ trả lỗi khi ctx bị huỷ.
func (c *Cache) Prefetch(ctx context.Context, t tree.Tree, paths []string, workers int) error {
	if workers <= 0 {
		workers = 16
	}
	ch := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range ch {
				if ctx.Err() != nil {
					continue
				}
				_, _ = c.Scan(t, p) // Scan tự bỏ qua việc đọc khi trúng cache; lỗi đọc để Scan sau báo
			}
		}()
	}
	for _, p := range paths {
		if lang(p) == "" {
			continue
		}
		select {
		case ch <- p:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(ch)
	wg.Wait()
	return ctx.Err()
}
