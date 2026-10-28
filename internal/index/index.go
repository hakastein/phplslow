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
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/ncruces/go-sqlite3"

	"github.com/hakastein/phplslow/internal/php"
)

// schemaDir names the schema and the link semantics of the index. An incompatible change of either gets a new name,
// so an index of another schema or with links resolved by other rules is never opened.
const schemaDir = "index-v2"

// parserModule is the module of the PHP parser; the binary may be linked with a module that replaces it.
const parserModule = "github.com/VKCOM/php-parser"

// indexDir is the directory of every index this binary builds: schemaDir and the parser module the binary is linked
// with. An index whose facts another parser extracted has the same snapshot of files, so it lives elsewhere and is
// never opened. A parser from a local directory has no version; the binary file itself names it then.
func indexDir() (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", errors.New("index: the binary carries no module information")
	}
	for _, dep := range info.Deps {
		if dep.Path != parserModule {
			continue
		}
		if dep.Replace != nil {
			dep = dep.Replace
		}
		sum := sha256.Sum256([]byte(dep.Path + "@" + dep.Version))
		if dep.Version == "" || dep.Version == "(devel)" {
			var err error
			if sum, err = executableSum(); err != nil {
				return "", err
			}
		}
		return schemaDir + "-" + hex.EncodeToString(sum[:6]), nil
	}
	return "", fmt.Errorf("index: the binary carries no version of %s", parserModule)
}

// executableSum is the sha256 of the running binary's file.
func executableSum() ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	exe, err := os.Executable()
	if err != nil {
		return sum, err
	}
	f, err := os.Open(exe)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

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
	dir, err := indexDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(cache, "phplslow", dir, hex.EncodeToString(sum[:16]), "index.sqlite"), nil
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
		same, n, err := ix.sameFiles(root, paths, logf)
		if err != nil {
			return err
		}
		if same {
			logf("phplslow: index reused: %d files", n)
			return nil
		}
	}
	n, err := ix.build(root, paths, logf)
	if err != nil {
		return err
	}
	logf("phplslow: index built: %d files", n)
	return nil
}

// phpFiles lists the .php files under root, relative, with forward slashes, sorted. Symlinks are followed, to files
// and to directories, also outside root; a file is named by its path through the symlink. A directory that is
// already on the way from root to it is not walked again. Only regular files are listed.
//
// An entry that cannot be walked is an error: the list would miss files without saying so. Only a broken symlink
// whose name does not end in .php is skipped: what it pointed at, a file or a directory, cannot be known. A symlink
// is broken when its target does not exist or its chain of symlinks does not end; any other error of following it
// is an error.
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
	if err := walkDir(root, "", entries, []string{canonical}, &paths, logf); err != nil {
		return nil, err
	}
	slices.Sort(paths)
	return paths, nil
}

// walkDir lists the .php files of the directory dir, whose path relative to the root is rel; chain holds the
// canonical paths of the directories from the root to dir.
func walkDir(dir, rel string, entries []fs.DirEntry, chain []string, paths *[]string, logf func(string, ...any)) error {
	for _, e := range entries {
		p, r := filepath.Join(dir, e.Name()), path.Join(rel, e.Name())
		mode := e.Type()
		if mode&fs.ModeSymlink != 0 {
			info, err := os.Stat(p) // never opens the target: a named pipe does not block it
			switch {
			case err == nil:
				mode = info.Mode().Type()
			case broken(err) && !strings.HasSuffix(e.Name(), ".php"):
				logf("phplslow: skip %s: %v", p, err)
				continue
			default:
				return err
			}
		}
		switch {
		case mode.IsDir():
			canonical, err := filepath.EvalSymlinks(p)
			if err != nil {
				return err
			}
			if slices.Contains(chain, canonical) {
				logf("phplslow: skip %s: a symlink cycle", p)
				continue
			}
			sub, err := os.ReadDir(p)
			if err != nil {
				return err
			}
			if err := walkDir(p, r, sub, append(chain[:len(chain):len(chain)], canonical), paths, logf); err != nil {
				return err
			}
		case mode.IsRegular() && strings.HasSuffix(e.Name(), ".php"):
			*paths = append(*paths, r)
		}
	}
	return nil
}

// broken reports whether err, from following a symlink, says that the symlink leads nowhere: its target does not
// exist or the chain of symlinks does not end.
func broken(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP)
}

// errNotRegular is the error of reading a name that is not a regular file.
var errNotRegular = errors.New("not a regular file")

// readFile reads a regular file. The file is opened without blocking and checked after opening, so a name that
// has become a named pipe, a socket or a device by then is not read: the error is errNotRegular.
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
		return nil, fmt.Errorf("%s: %w", name, errNotRegular)
	}
	return io.ReadAll(f)
}

// snapshotFile reads the file p of the Workspace, relative to root, and hashes it for the snapshot. A name that is
// no longer a regular file is logged and is not part of the snapshot: ok is false. Any other error is returned:
// the snapshot cannot be known without the file.
func snapshotFile(root, p string, logf func(string, ...any)) (src []byte, sum [sha256.Size]byte, ok bool, err error) {
	src, err = readFile(filepath.Join(root, filepath.FromSlash(p)))
	if errors.Is(err, errNotRegular) {
		logf("phplslow: skip %s: %v", p, err)
		return nil, sum, false, nil
	}
	if err != nil {
		return nil, sum, false, err
	}
	return src, sha256.Sum256(src), true, nil
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

// sameFiles reports whether the stored snapshot equals the files on disk and how many files it holds. A file that
// cannot be read is an error, never a match.
func (ix *Index) sameFiles(root string, paths []string, logf func(string, ...any)) (bool, int, error) {
	stored := map[string]string{}
	stmt, _, err := ix.conn.Prepare(`SELECT path, hex(hash) FROM files`)
	if err != nil {
		return false, 0, err
	}
	for stmt.Step() {
		stored[stmt.ColumnText(0)] = strings.ToLower(stmt.ColumnText(1))
	}
	if err := errors.Join(stmt.Err(), stmt.Close()); err != nil {
		return false, 0, err
	}
	read := 0
	for _, p := range paths {
		_, sum, ok, err := snapshotFile(root, p, logf)
		if err != nil {
			return false, 0, err
		}
		if !ok {
			continue
		}
		read++
		if stored[p] != hex.EncodeToString(sum[:]) {
			return false, 0, nil
		}
	}
	return read == len(stored), read, nil
}

// build replaces the whole index in one transaction and returns the number of files indexed. A build that does
// not finish, also on a file that cannot be read, leaves the previous state.
func (ix *Index) build(root string, paths []string, logf func(string, ...any)) (n int, err error) {
	tx, err := ix.conn.BeginImmediate()
	if err != nil {
		return 0, err
	}
	defer tx.End(&err)

	if err := ix.conn.Exec(`
		DELETE FROM meta; DELETE FROM files; DELETE FROM symbols; DELETE FROM links;
		CREATE TEMP TABLE calls (key TEXT NOT NULL, file INTEGER NOT NULL,
			sl INTEGER NOT NULL, sc INTEGER NOT NULL, el INTEGER NOT NULL, ec INTEGER NOT NULL);`); err != nil {
		return 0, err
	}
	insFile, _, err := ix.conn.Prepare(`INSERT INTO files (path, hash) VALUES (?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insFile.Close()
	insSymbol, _, err := ix.conn.Prepare(`INSERT INTO symbols (key, file, sl, sc, el, ec) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insSymbol.Close()
	insCall, _, err := ix.conn.Prepare(
		`INSERT INTO temp.calls (key, file, sl, sc, el, ec) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insCall.Close()

	for _, p := range paths {
		src, sum, ok, err := snapshotFile(root, p, logf)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		n++
		if err := exec(insFile, p, sum[:]); err != nil {
			return 0, fmt.Errorf("%s: %w", p, err)
		}
		file := ix.conn.LastInsertRowID()
		facts, syntax := php.Parse(src)
		for _, e := range syntax {
			logf("phplslow: %s: %s", p, e)
		}
		for _, m := range facts.Methods {
			if err := exec(insSymbol, m.Key, file, m.Name); err != nil {
				return 0, fmt.Errorf("%s: %w", p, err)
			}
		}
		for _, c := range facts.Calls {
			if err := exec(insCall, c.Key, file, c.Name); err != nil {
				return 0, fmt.Errorf("%s: %w", p, err)
			}
		}
	}
	// A call becomes a link only when its target Symbol is declared somewhere in the Workspace.
	return n, ix.conn.Exec(`
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
