// Package index keeps the facts of a Workspace in SQLite on disk: the files it was built from, the method Symbols
// and the resolved links «place → Symbol». Links are resolved while indexing; a query only reads them.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

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

// indexPath is the index file of root. phplslow chooses it: a directory per root under the user's cache directory.
func indexPath(root string) (string, error) {
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
	file, err := indexPath(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	conn, err := sqlite3.Open(file)
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

// phpFiles lists the .php files under root, relative, with forward slashes, sorted. Symlinks are followed, to files
// and to directories, also outside root; a file is named by its path through the symlink. A directory that is
// already on the way from root to it is not walked again. Only regular files are listed.
func phpFiles(root string, logf func(string, ...any)) ([]string, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var paths []string
	walkDir(root, "", entries, []string{canonical}, &paths, logf)
	slices.Sort(paths)
	return paths, nil
}

// walkDir lists the .php files of the directory dir, whose path relative to the root is rel; chain holds the
// canonical paths of the directories from the root to dir.
func walkDir(dir, rel string, entries []fs.DirEntry, chain []string, paths *[]string, logf func(string, ...any)) {
	for _, e := range entries {
		p, r := filepath.Join(dir, e.Name()), path.Join(rel, e.Name())
		mode := e.Type()
		if mode&fs.ModeSymlink != 0 {
			info, err := os.Stat(p) // never opens the target: a named pipe does not block it
			if err != nil {
				logf("phplslow: skip %s: %v", p, err)
				continue
			}
			mode = info.Mode().Type()
		}
		switch {
		case mode.IsDir():
			canonical, err := filepath.EvalSymlinks(p)
			if err != nil {
				logf("phplslow: skip %s: %v", p, err)
				continue
			}
			if slices.Contains(chain, canonical) {
				logf("phplslow: skip %s: a symlink cycle", p)
				continue
			}
			sub, err := os.ReadDir(p)
			if err != nil {
				logf("phplslow: skip %s: %v", p, err)
				continue
			}
			walkDir(p, r, sub, append(chain[:len(chain):len(chain)], canonical), paths, logf)
		case mode.IsRegular() && strings.HasSuffix(e.Name(), ".php"):
			*paths = append(*paths, r)
		}
	}
}

// readFile reads a regular file. The file is opened without blocking and checked after opening, so a name that
// has become a named pipe, a socket or a device by then is not read.
func readFile(name string) ([]byte, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", name)
	}
	return io.ReadAll(f)
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
		src, err := readFile(filepath.Join(root, filepath.FromSlash(p)))
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
			sl INTEGER NOT NULL, sc INTEGER NOT NULL, el INTEGER NOT NULL, ec INTEGER NOT NULL, global TEXT NOT NULL);
		CREATE TEMP TABLE reached (name TEXT);`); err != nil {
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
	insCall, _, err := ix.conn.Prepare(
		`INSERT INTO temp.calls (key, file, sl, sc, el, ec, global) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insCall.Close()
	// A row of reached is a top-level variable some file reaches by name; a NULL name stands for any.
	insReached, _, err := ix.conn.Prepare(`INSERT INTO temp.reached (name) VALUES (?)`)
	if err != nil {
		return err
	}
	defer insReached.Close()

	for _, p := range paths {
		src, err := readFile(filepath.Join(root, filepath.FromSlash(p)))
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
			if err := exec(insCall, c.Key, file, c.Name, c.Global); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		for _, name := range facts.Globals {
			if err := exec(insReached, name); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		if facts.AnyGlobal {
			if err := exec(insReached, nil); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
	}
	// A call becomes a link only when its target Symbol is declared somewhere in the Workspace, and, on a top-level
	// variable, only when no file of the Workspace reaches that variable through `global` or `$GLOBALS`.
	return ix.conn.Exec(`
		INSERT INTO links (key, file, sl, sc, el, ec)
			SELECT key, file, sl, sc, el, ec FROM temp.calls c
			WHERE EXISTS (SELECT 1 FROM symbols s WHERE s.key = c.key)
			AND (c.global = '' OR NOT EXISTS (
				SELECT 1 FROM temp.reached r WHERE r.name IS NULL OR r.name = c.global));
		DROP TABLE temp.calls;
		DROP TABLE temp.reached;
		INSERT INTO meta (key, value) VALUES ('complete', '1');`)
}

// exec binds args in order and runs stmt once. A php.Range binds as four integers, nil as NULL.
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
		case nil:
			err = stmt.BindNull(i)
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
