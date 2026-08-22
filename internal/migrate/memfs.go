package migrate

import (
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// memFS is a minimal read-only in-memory filesystem holding a flat set of files
// keyed by their slash-separated path. It exists so the tenant migration runner
// can hand goose an fs.FS whose contents have the embedding-dimension
// placeholder substituted, without pulling testing/fstest into the production
// build. It implements the fs.FS, fs.ReadDirFS and fs.ReadFileFS behaviour goose
// relies on to enumerate and read migrations.
type memFS map[string][]byte

var (
	_ fs.FS         = memFS(nil)
	_ fs.ReadDirFS  = memFS(nil)
	_ fs.ReadFileFS = memFS(nil)
)

// Open implements fs.FS for files and directories present in the map.
func (m memFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return &memDir{name: ".", entries: m.dirEntries(".")}, nil
	}
	if data, ok := m[name]; ok {
		return &memFile{name: path.Base(name), data: data}, nil
	}
	if entries := m.dirEntries(name); len(entries) > 0 {
		return &memDir{name: path.Base(name), entries: entries}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadFile implements fs.ReadFileFS.
func (m memFS) ReadFile(name string) ([]byte, error) {
	data, ok := m[name]
	if !ok {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// ReadDir implements fs.ReadDirFS.
func (m memFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries := m.dirEntries(name)
	if name != "." && len(entries) == 0 {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	return entries, nil
}

// dirEntries returns the immediate children (files and subdirs) of dir.
func (m memFS) dirEntries(dir string) []fs.DirEntry {
	prefix := ""
	if dir != "." {
		prefix = dir + "/"
	}
	seen := map[string]bool{}
	var out []fs.DirEntry
	for name := range m {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			child := rest[:i]
			if !seen[child] {
				seen[child] = true
				out = append(out, dirDirEntry(child))
			}
			continue
		}
		if !seen[rest] {
			seen[rest] = true
			out = append(out, fileDirEntry{name: rest, size: int64(len(m[name]))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// memFile is an open regular file.
type memFile struct {
	name   string
	data   []byte
	offset int
}

func (f *memFile) Stat() (fs.FileInfo, error) {
	return memFileInfo{name: f.name, size: int64(len(f.data))}, nil
}

func (f *memFile) Read(p []byte) (int, error) {
	if f.offset >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += n
	return n, nil
}

func (f *memFile) Close() error { return nil }

// memDir is an open directory.
type memDir struct {
	name    string
	entries []fs.DirEntry
	offset  int
}

func (d *memDir) Stat() (fs.FileInfo, error) {
	return memFileInfo{name: d.name, dir: true}, nil
}

func (d *memDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}

func (d *memDir) Close() error { return nil }

func (d *memDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		rest := d.entries[d.offset:]
		d.offset = len(d.entries)
		return rest, nil
	}
	if d.offset >= len(d.entries) {
		return nil, io.EOF
	}
	end := d.offset + n
	if end > len(d.entries) {
		end = len(d.entries)
	}
	rest := d.entries[d.offset:end]
	d.offset = end
	return rest, nil
}

// fileDirEntry / dirDirEntry are fs.DirEntry implementations for files and dirs.
type fileDirEntry struct {
	name string
	size int64
}

func (e fileDirEntry) Name() string      { return e.name }
func (e fileDirEntry) IsDir() bool       { return false }
func (e fileDirEntry) Type() fs.FileMode { return 0 }
func (e fileDirEntry) Info() (fs.FileInfo, error) {
	return memFileInfo{name: e.name, size: e.size}, nil
}

type dirDirEntry string

func (e dirDirEntry) Name() string               { return string(e) }
func (e dirDirEntry) IsDir() bool                { return true }
func (e dirDirEntry) Type() fs.FileMode          { return fs.ModeDir }
func (e dirDirEntry) Info() (fs.FileInfo, error) { return memFileInfo{name: string(e), dir: true}, nil }

// memFileInfo is a static fs.FileInfo.
type memFileInfo struct {
	name string
	size int64
	dir  bool
}

func (i memFileInfo) Name() string { return i.name }
func (i memFileInfo) Size() int64  { return i.size }
func (i memFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i memFileInfo) ModTime() time.Time { return time.Time{} }
func (i memFileInfo) IsDir() bool        { return i.dir }
func (i memFileInfo) Sys() any           { return nil }
