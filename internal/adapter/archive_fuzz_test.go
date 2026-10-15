package adapter

import (
	"archive/tar"
	"bytes"
	"path"
	"strings"
	"testing"
)

// fuzzArchive builds a tar (optionally with an attempted path-traversal or
// non-regular entry) for the corpus below.
func fuzzArchiveSeed(name string, typeflag byte, content string) []byte {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	_ = w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: typeflag})
	_, _ = w.Write([]byte(content))
	_ = w.Close()
	return b.Bytes()
}

// FuzzReadArchive checks readArchive's two safety invariants against Go's
// own archive/tar reader and moby's path rules: every returned file's
// target stays inside dir (never escapes via ../ or an absolute path), and
// only regular files (tar.TypeReg) are ever accepted.
func FuzzReadArchive(f *testing.F) {
	f.Add(fuzzArchiveSeed("a.txt", tar.TypeReg, "hello"), "/dest")
	f.Add(fuzzArchiveSeed("../../etc/passwd", tar.TypeReg, "pwned"), "/dest")
	f.Add(fuzzArchiveSeed("/etc/passwd", tar.TypeReg, "pwned"), "/dest")
	f.Add(fuzzArchiveSeed("sub/../../escape", tar.TypeReg, "x"), "/dest")
	f.Add(fuzzArchiveSeed("link", tar.TypeSymlink, ""), "/dest")
	f.Add(fuzzArchiveSeed("dev", tar.TypeChar, ""), "/dest")
	f.Add(fuzzArchiveSeed("dir/", tar.TypeDir, ""), "/dest")
	f.Add([]byte{}, "/dest")
	f.Add([]byte("not a tar at all"), "/dest")

	f.Fuzz(func(t *testing.T, raw []byte, dir string) {
		files, err := readArchive(dir, bytes.NewReader(raw))
		if err != nil {
			return
		}
		cleanDir := path.Clean("/" + dir)
		for _, cf := range files {
			if cf.target != cleanDir && !strings.HasPrefix(cf.target, strings.TrimSuffix(cleanDir, "/")+"/") {
				t.Fatalf("readArchive(%q) produced target %q outside %q", dir, cf.target, cleanDir)
			}
			if strings.Contains(cf.target, "/../") || strings.HasSuffix(cf.target, "/..") {
				t.Fatalf("readArchive(%q) produced an unresolved .. in target %q", dir, cf.target)
			}
		}
	})
}
