package store

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

type tarEntry struct {
	name string
	body string
	link string // non-empty → a symlink with this target
}

func rawTar(entries []tarEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755}
		if e.link != "" {
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.link
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}
		tw.WriteHeader(hdr)
		if e.link == "" {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return buf.Bytes()
}

func TestExtractTarRejectsUnsafeArchives(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
		wantErr bool
	}{
		{"normal", []tarEntry{{name: "a/b.txt", body: "hi"}}, false},
		{"parent escape", []tarEntry{{name: "../evil", body: "x"}}, true},
		{"absolute path", []tarEntry{{name: "/etc/evil", body: "x"}}, true},
		{"nested escape", []tarEntry{{name: "ok/../../evil", body: "x"}}, true},
		{"escaping symlink", []tarEntry{{name: "link", link: "../../etc/passwd"}}, true},
		{"absolute symlink", []tarEntry{{name: "link", link: "/etc/passwd"}}, true},
		{"empty archive", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			err := ExtractTar(bytes.NewReader(rawTar(tc.entries)), dst)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ExtractTar err = %v, wantErr = %v", err, tc.wantErr)
			}
			// Nothing may ever land outside the destination.
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(dst), "evil")); statErr == nil {
				t.Fatal("an entry escaped the extraction root")
			}
		})
	}
}

// ExtractTar accepts raw (uncompressed) tar as well as gzip.
func TestExtractTarAcceptsRawTar(t *testing.T) {
	dst := t.TempDir()
	if err := ExtractTar(bytes.NewReader(rawTar([]tarEntry{{name: "f.txt", body: "raw"}})), dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "raw" {
		t.Fatalf("extracted %q", got)
	}
}
