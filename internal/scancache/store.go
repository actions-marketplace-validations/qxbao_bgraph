package scancache

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/model"

	_ "modernc.org/sqlite" // trình điều khiển thuần Go, không cgo
)

// schemaVersion: đổi khi đổi bố cục bảng hoặc mã hoá blob.
const schemaVersion = "1"

const dbName = "scan.sqlite"

// dsn dựng chuỗi kết nối. Pragma đặt theo kết nối qua _pragma (modernc chạy lại cho mỗi kết nối mới trong pool):
// WAL cho phép nhiều tiến trình đọc cùng lúc ghi; synchronous=NORMAL an toàn với WAL (mất tối đa giao dịch cuối khi
// mất điện, cache chấp nhận được); busy_timeout chờ khoá thay vì lỗi ngay; cache_size 64 MB theo phép đo 0a (nhanh
// hơn khi chèn); _txlock=immediate để giao dịch lấy khoá ghi ngay từ đầu, tránh SQLITE_BUSY_SNAPSHOT.
func dsn(file string) string {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)", "cache_size(-65536)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	return "file:" + filepath.ToSlash(file) + "?" + q.Encode()
}

// Open mở (hoặc tạo) <dir>/scan.sqlite. Lỗi kho KHÔNG làm Open thất bại: Cache hạ về chỉ-bộ-nhớ kèm Warnings().
// Tệp hỏng được đổi tên sang scan.sqlite.corrupt-<unix> rồi tạo lại. Chỉ trả lỗi khi dir rỗng.
func Open(dir string) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("scancache: thư mục rỗng")
	}
	c := New()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.warn("scancache: không tạo được %s, dùng cache bộ nhớ: %v", dir, err)
		return c, nil
	}
	file := filepath.Join(dir, dbName)
	err := c.attach(file)
	if err != nil && isCorrupt(err) {
		aside := file + ".corrupt-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		c.closeDB()
		if rerr := os.Rename(file, aside); rerr == nil {
			_ = os.Remove(file + "-wal")
			_ = os.Remove(file + "-shm")
			c.warn("scancache: cache hỏng (%v), đã đổi tên thành %s và tạo lại", err, filepath.Base(aside))
			err = c.attach(file)
		} else {
			c.warn("scancache: cache hỏng nhưng không đổi tên được: %v", rerr)
		}
	}
	if err != nil {
		c.closeDB()
		c.warn("scancache: không dùng được kho bền, dùng cache bộ nhớ: %v", err)
	}
	return c, nil
}

// isCorrupt: lỗi SQLite báo tệp không phải DB / hỏng (CORRUPT=11, NOTADB=26).
func isCorrupt(err error) bool {
	s := err.Error()
	for _, sub := range []string{"SQLITE_CORRUPT", "SQLITE_NOTADB", "not a database", "malformed", "(26)", "(11)"} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// attach mở kết nối, kiểm/khởi tạo lược đồ và chuẩn bị câu lệnh đọc.
func (c *Cache) attach(file string) error {
	db, err := sql.Open("sqlite", dsn(file))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(8)
	c.db = db
	// Ép mở thật + đọc trang đầu để lộ tệp hỏng sớm.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID`); err != nil {
		return err
	}
	tx, err := db.Begin() // immediate: tuần tự hoá việc khởi tạo giữa các tiến trình
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sv, scv string
	_ = tx.QueryRow(`SELECT v FROM meta WHERE k='schema_version'`).Scan(&sv)
	_ = tx.QueryRow(`SELECT v FROM meta WHERE k='scanner_version'`).Scan(&scv)
	if sv != schemaVersion || scv != ScannerVersion {
		// Lệch phiên bản (hoặc DB mới): bỏ bảng cũ, dựng lại.
		for _, q := range []string{
			`DROP TABLE IF EXISTS scan`,
			`CREATE TABLE scan(key TEXT PRIMARY KEY, result BLOB) WITHOUT ROWID`,
			`DELETE FROM meta`,
			`INSERT INTO meta(k,v) VALUES('schema_version','` + schemaVersion + `'),('scanner_version','` + ScannerVersion + `')`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Chạm bảng scan để chắc nó đọc được (bắt hỏng trang).
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM (SELECT 1 FROM scan LIMIT 1)`).Scan(&n); err != nil {
		return err
	}
	c.get, err = db.Prepare(`SELECT result FROM scan WHERE key=?`)
	return err
}

func (c *Cache) closeDB() {
	if c.get != nil {
		c.get.Close()
		c.get = nil
	}
	if c.db != nil {
		c.db.Close()
		c.db = nil
	}
}

// disableDB hạ về chỉ-bộ-nhớ (gọi khi giữ c.mu).
func (c *Cache) disableDB(err error) {
	if c.db == nil {
		return
	}
	c.warn("scancache: lỗi kho bền, chuyển sang chỉ bộ nhớ: %v", err)
	c.closeDB()
}

// Flush ghi mọi kết quả mới xuống kho trong MỘT giao dịch. Không có kho thì không làm gì. Lỗi ghi chỉ thành cảnh báo.
func (c *Cache) Flush() error {
	c.mu.Lock()
	if c.db == nil || len(c.dirty) == 0 {
		c.mu.Unlock()
		return nil
	}
	db := c.db
	rows := make(map[string]model.ScanResult, len(c.dirty))
	for k := range c.dirty {
		rows[k] = c.m[k]
	}
	c.dirty = map[string]struct{}{}
	c.mu.Unlock()

	err := func() error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		st, err := tx.Prepare(`INSERT OR REPLACE INTO scan(key,result) VALUES(?,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for k, r := range rows {
			if _, err := st.Exec(k, encode(r)); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	if err != nil {
		c.mu.Lock()
		c.warn("scancache: ghi cache thất bại (bỏ qua): %v", err)
		c.mu.Unlock()
	}
	return nil
}

// Close ghi nốt (Flush) rồi đóng kho. Cache vẫn dùng được như chỉ-bộ-nhớ sau Close.
func (c *Cache) Close() error {
	_ = c.Flush()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeDB()
	return nil
}

// loadAll nạp toàn bộ kho vào map (chỉ để đo so với tra từng khoá; không dùng mặc định).
func (c *Cache) loadAll() (int, error) {
	c.mu.Lock()
	db := c.db
	c.mu.Unlock()
	if db == nil {
		return 0, errors.New("không có kho")
	}
	rows, err := db.Query(`SELECT key, result FROM scan`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var k string
		var b []byte
		if err := rows.Scan(&k, &b); err != nil {
			return n, err
		}
		r, err := decode(b)
		if err != nil {
			continue
		}
		c.mu.Lock()
		c.m[k] = r
		c.mu.Unlock()
		n++
	}
	return n, rows.Err()
}
