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

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

// schemaVersion: change when the table layout or blob encoding changes.
const schemaVersion = "1"

const dbName = "scan.sqlite"

// dsn builds the connection string. Pragmas are set per connection via _pragma (modernc re-runs them for each new connection in the pool):
// WAL lets multiple processes read while one writes; synchronous=NORMAL is safe with WAL (at most the last transaction is lost on
// power loss, acceptable for a cache); busy_timeout waits for the lock instead of failing immediately; cache_size 64 MB per the 0a measurements (faster
// on insert); _txlock=immediate so transactions take the write lock up front, avoiding SQLITE_BUSY_SNAPSHOT.
func dsn(file string) string {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)", "cache_size(-65536)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	return "file:" + filepath.ToSlash(file) + "?" + q.Encode()
}

// Open opens (or creates) <dir>/scan.sqlite. Store errors do NOT make Open fail: the Cache degrades to memory-only with Warnings().
// A corrupt file is renamed to scan.sqlite.corrupt-<unix> and recreated. Returns an error only when dir is empty.
func Open(dir string) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("scancache: empty directory")
	}
	c := New()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.warn("scancache: cannot create %s, using in-memory cache: %v", dir, err)
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
			c.warn("scancache: cache is corrupt (%v), renamed to %s and recreated", err, filepath.Base(aside))
			err = c.attach(file)
		} else {
			c.warn("scancache: cache is corrupt and could not be renamed: %v", rerr)
		}
	}
	if err != nil {
		c.closeDB()
		c.warn("scancache: durable store unusable, using in-memory cache: %v", err)
	}
	return c, nil
}

// isCorrupt: the SQLite error says the file is not a DB / corrupt (CORRUPT=11, NOTADB=26).
func isCorrupt(err error) bool {
	s := err.Error()
	for _, sub := range []string{"SQLITE_CORRUPT", "SQLITE_NOTADB", "not a database", "malformed", "(26)", "(11)"} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// attach opens the connection, checks/initializes the schema and prepares the read statement.
func (c *Cache) attach(file string) error {
	db, err := sql.Open("sqlite", dsn(file))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(8)
	c.db = db
	// Force a real open + read of the first page to surface a corrupt file early.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID`); err != nil {
		return err
	}
	tx, err := db.Begin() // immediate: serializes initialization across processes
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sv, scv string
	_ = tx.QueryRow(`SELECT v FROM meta WHERE k='schema_version'`).Scan(&sv)
	_ = tx.QueryRow(`SELECT v FROM meta WHERE k='scanner_version'`).Scan(&scv)
	if sv != schemaVersion || scv != ScannerVersion {
		// Version mismatch (or new DB): drop the old table and rebuild.
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
	// Touch the scan table to make sure it is readable (catches page corruption).
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

// disableDB degrades to memory-only (call while holding c.mu).
func (c *Cache) disableDB(err error) {
	if c.db == nil {
		return
	}
	c.warn("scancache: durable store error, switching to memory-only: %v", err)
	c.closeDB()
}

// Flush writes all new results to the store in ONE transaction. Does nothing without a store. Write errors become warnings only.
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
		c.warn("scancache: cache write failed (ignored): %v", err)
		c.mu.Unlock()
	}
	return nil
}

// Close flushes, then closes the store. The Cache remains usable as memory-only after Close.
func (c *Cache) Close() error {
	_ = c.Flush()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeDB()
	return nil
}

// loadAll loads the whole store into the map (only for measurement against per-key lookup; not the default).
func (c *Cache) loadAll() (int, error) {
	c.mu.Lock()
	db := c.db
	c.mu.Unlock()
	if db == nil {
		return 0, errors.New("no store")
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
