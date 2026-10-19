// Package index keeps the facts of a Workspace in SQLite on disk: the files it was built from, the method Symbols
// and the resolved links «place → Symbol». Links are resolved while indexing; a query only reads them.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ncruces/go-sqlite3"

	"github.com/hakastein/phplslow/internal/php"
)

// schemaDir names the layout of the index. An incompatible layout gets a new name, so an index of another layout is
// never opened.
const schemaDir = "index-v1"

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS files (id INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE, hash BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS symbols (
	key TEXT NOT NULL, file INTEGER NOT NULL,
	sl INTEGER NOT NULL, sc INTEGER NOT NULL, el INTEGER NOT NULL, ec INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS symbols_key ON symbols (key);
CREATE INDEX IF NOT EXISTS symbols_place ON symbols (file, sl);
CREATE TABLE IF NOT EXISTS links (
	key TEXT NOT NULL, file INTEGER NOT NULL,
	sl INTEGER NOT NULL, sc INTEGER NOT NULL, el INTEGER NOT NULL, ec INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS links_key ON links (key);
CREATE INDEX IF NOT EXISTS links_place ON links (file, sl);
`

// Location is a range in a file of the Workspace; Path is relative to the Workspace root with forward slashes.
type Location struct {
	Path  string
	Range php.Range
}

// Index is the index of one Workspace root.
type Index struct {
	mu   sync.Mutex
	conn *sqlite3.Conn
}

// Path is the index file of root. phplslow chooses it: a directory per root under the user's cache directory.
func Path(root string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(cache, "phplslow", schemaDir, hex.EncodeToString(sum[:16]), "index.sqlite"), nil
}

// Open opens the index of root and makes it describe the files under root. An index whose snapshot of files
// («path → hash») equals the files on disk is used as it is; otherwise the whole index is built again.
// logf receives diagnostic lines; they are not a protocol.
func Open(root string, logf func(format string, args ...any)) (*Index, error) {
	path, err := Path(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	conn, err := sqlite3.Open(path)
	if err != nil {
		return nil, err
	}
	ix := &Index{conn: conn}
	if err := ix.prepare(root, logf); err != nil {
		conn.Close()
		return nil, err
	}
	return ix, nil
}

// Close closes the index file.
func (ix *Index) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.conn.Close()
}

func (ix *Index) prepare(root string, logf func(string, ...any)) error {
	if err := ix.conn.Exec(`PRAGMA busy_timeout = 60000;` + schema); err != nil {
		return err
	}
	paths, err := phpFiles(root, logf)
	if err != nil {
		return err
	}
	complete, err := ix.complete()
	if err != nil {
		return err
	}
	if complete {
		same, err := ix.sameFiles(root, paths, logf)
		if err != nil {
			return err
		}
		if same {
			logf("phplslow: index reused: %d files", len(paths))
			return nil
		}
	}
	if err := ix.build(root, paths, logf); err != nil {
		return err
	}
	logf("phplslow: index built: %d files", len(paths))
	return nil
}

// phpFiles lists the .php files under root, relative, with forward slashes, sorted.
func phpFiles(root string, logf func(string, ...any)) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			logf("phplslow: skip %s: %v", p, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".php") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(paths)
	return paths, err
}

func (ix *Index) complete() (bool, error) {
	stmt, _, err := ix.conn.Prepare(`SELECT 1 FROM meta WHERE key = 'complete'`)
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	found := stmt.Step()
	return found, stmt.Err()
}

// sameFiles reports whether the stored snapshot equals the files on disk. A file that cannot be read is not part
// of the snapshot.
func (ix *Index) sameFiles(root string, paths []string, logf func(string, ...any)) (bool, error) {
	stored := map[string]string{}
	stmt, _, err := ix.conn.Prepare(`SELECT path, hex(hash) FROM files`)
	if err != nil {
		return false, err
	}
	for stmt.Step() {
		stored[stmt.ColumnText(0)] = strings.ToLower(stmt.ColumnText(1))
	}
	if err := errors.Join(stmt.Err(), stmt.Close()); err != nil {
		return false, err
	}
	read := 0
	for _, p := range paths {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			logf("phplslow: skip %s: %v", p, err)
			continue
		}
		read++
		sum := sha256.Sum256(src)
		if stored[p] != hex.EncodeToString(sum[:]) {
			return false, nil
		}
	}
	return read == len(stored), nil
}

// build replaces the whole index in one transaction: a build that does not finish leaves the previous state.
func (ix *Index) build(root string, paths []string, logf func(string, ...any)) (err error) {
	tx, err := ix.conn.BeginImmediate()
	if err != nil {
		return err
	}
	defer tx.End(&err)

	if err := ix.conn.Exec(`
		DELETE FROM meta; DELETE FROM files; DELETE FROM symbols; DELETE FROM links;
		CREATE TEMP TABLE calls (key TEXT NOT NULL, file INTEGER NOT NULL,
			sl INTEGER NOT NULL, sc INTEGER NOT NULL, el INTEGER NOT NULL, ec INTEGER NOT NULL);`); err != nil {
		return err
	}
	insFile, _, err := ix.conn.Prepare(`INSERT INTO files (path, hash) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer insFile.Close()
	insSymbol, _, err := ix.conn.Prepare(`INSERT INTO symbols (key, file, sl, sc, el, ec) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insSymbol.Close()
	insCall, _, err := ix.conn.Prepare(`INSERT INTO temp.calls (key, file, sl, sc, el, ec) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insCall.Close()

	for _, p := range paths {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			logf("phplslow: skip %s: %v", p, err)
			continue
		}
		sum := sha256.Sum256(src)
		if err := exec(insFile, p, sum[:]); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		file := ix.conn.LastInsertRowID()
		facts, syntax := php.Parse(src)
		for _, e := range syntax {
			logf("phplslow: %s: %s", p, e)
		}
		for _, m := range facts.Methods {
			if err := exec(insSymbol, m.Key, file, m.Name); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		for _, c := range facts.Calls {
			if err := exec(insCall, c.Key, file, c.Name); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
	}
	// A call becomes a link only when its target Symbol is declared somewhere in the Workspace.
	return ix.conn.Exec(`
		INSERT INTO links (key, file, sl, sc, el, ec)
			SELECT key, file, sl, sc, el, ec FROM temp.calls c
			WHERE EXISTS (SELECT 1 FROM symbols s WHERE s.key = c.key);
		DROP TABLE temp.calls;
		INSERT INTO meta (key, value) VALUES ('complete', '1');`)
}

// exec binds args in order and runs stmt once. A php.Range binds as four integers.
func exec(stmt *sqlite3.Stmt, args ...any) error {
	i := 1
	for _, a := range args {
		var err error
		switch v := a.(type) {
		case string:
			err = stmt.BindText(i, v)
		case []byte:
			err = stmt.BindBlob(i, v)
		case int64:
			err = stmt.BindInt64(i, v)
		case int:
			err = stmt.BindInt(i, v)
		case php.Range:
			err = errors.Join(stmt.BindInt(i, v.Start.Line), stmt.BindInt(i+1, v.Start.Character),
				stmt.BindInt(i+2, v.End.Line), stmt.BindInt(i+3, v.End.Character))
			i += 3
		default:
			err = fmt.Errorf("index: cannot bind %T", a)
		}
		if err != nil {
			return err
		}
		i++
	}
	stmt.Step()
	return errors.Join(stmt.Err(), stmt.Reset())
}

// Definition is the declarations of the Symbol at pos in file: the one a link there points to, or the one
// declared there.
func (ix *Index) Definition(file string, pos php.Position) ([]Location, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	key, err := ix.symbolAt(file, pos)
	if err != nil || key == "" {
		return nil, err
	}
	return ix.locations(`SELECT f.path, s.sl, s.sc, s.el, s.ec FROM symbols s JOIN files f ON f.id = s.file
		WHERE s.key = ? ORDER BY f.path, s.sl, s.sc`, key)
}

// References is every link to the Symbol at pos in file and, with declaration, its declarations.
func (ix *Index) References(file string, pos php.Position, declaration bool) ([]Location, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	key, err := ix.symbolAt(file, pos)
	if err != nil || key == "" {
		return nil, err
	}
	refs, err := ix.locations(`SELECT f.path, l.sl, l.sc, l.el, l.ec FROM links l JOIN files f ON f.id = l.file
		WHERE l.key = ? ORDER BY f.path, l.sl, l.sc`, key)
	if err != nil || !declaration {
		return refs, err
	}
	decls, err := ix.locations(`SELECT f.path, s.sl, s.sc, s.el, s.ec FROM symbols s JOIN files f ON f.id = s.file
		WHERE s.key = ? ORDER BY f.path, s.sl, s.sc`, key)
	return append(decls, refs...), err
}

// symbolAt is the key of the Symbol a link at pos points to or that is declared at pos; "" when there is none.
func (ix *Index) symbolAt(file string, pos php.Position) (string, error) {
	for _, q := range []string{
		`SELECT l.key FROM links l JOIN files f ON f.id = l.file
			WHERE f.path = ?1 AND (l.sl, l.sc) <= (?2, ?3) AND (?2, ?3) <= (l.el, l.ec) LIMIT 1`,
		`SELECT s.key FROM symbols s JOIN files f ON f.id = s.file
			WHERE f.path = ?1 AND (s.sl, s.sc) <= (?2, ?3) AND (?2, ?3) <= (s.el, s.ec) LIMIT 1`,
	} {
		stmt, _, err := ix.conn.Prepare(q)
		if err != nil {
			return "", err
		}
		key := ""
		err = errors.Join(stmt.BindText(1, file), stmt.BindInt(2, pos.Line), stmt.BindInt(3, pos.Character))
		if err == nil && stmt.Step() {
			key = stmt.ColumnText(0)
		}
		if err := errors.Join(err, stmt.Err(), stmt.Close()); err != nil {
			return "", err
		}
		if key != "" {
			return key, nil
		}
	}
	return "", nil
}

func (ix *Index) locations(q, key string) ([]Location, error) {
	stmt, _, err := ix.conn.Prepare(q)
	if err != nil {
		return nil, err
	}
	var out []Location
	err = stmt.BindText(1, key)
	for err == nil && stmt.Step() {
		out = append(out, Location{Path: stmt.ColumnText(0), Range: php.Range{
			Start: php.Position{Line: stmt.ColumnInt(1), Character: stmt.ColumnInt(2)},
			End:   php.Position{Line: stmt.ColumnInt(3), Character: stmt.ColumnInt(4)},
		}})
	}
	return out, errors.Join(err, stmt.Err(), stmt.Close())
}
