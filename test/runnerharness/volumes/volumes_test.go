package volumes_test

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/test/runnerharness/volumes"
)

// What Pack writes, Unpack puts back, with the modes named.
func TestUnpackPutsBackWhatPackWrote(t *testing.T) {
	t.Parallel()
	uid, gid := os.Getuid(), os.Getgid()
	stream, err := volumes.Pack([]volumes.Entry{
		{Volume: "work", Path: ".", Directory: true, Mode: 0o770, UID: uid, GID: gid},
		{Volume: "plan", Path: ".", Directory: true, Mode: 0o750, UID: uid, GID: gid},
		{Volume: "plan", Path: "000.plan", Data: []byte("first"), Mode: 0o440, UID: uid, GID: gid},
		{Volume: "plan", Path: "nested/001.plan", Data: []byte("second"), Mode: 0o400, UID: uid, GID: gid},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := volumes.Unpack(bytes.NewReader(stream), root); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]struct {
		mode os.FileMode
		data string
	}{
		"plan/000.plan":        {0o440, "first"},
		"plan/nested/001.plan": {0o400, "second"},
	} {
		info, err := os.Stat(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want.mode {
			t.Errorf("%s has mode %o, want %o", file, info.Mode().Perm(), want.mode)
		}
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want.data {
			t.Errorf("%s holds %q", file, data)
		}
	}
	for directory, want := range map[string]os.FileMode{"work": 0o770, "plan": 0o750} {
		info, err := os.Stat(filepath.Join(root, directory))
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != want {
			t.Errorf("%s is %v, want a directory with mode %o", directory, info.Mode(), want)
		}
	}
}

func TestUnpackRefusesWhatIsNotAVolumesFile(t *testing.T) {
	t.Parallel()
	for name, header := range map[string]*tar.Header{
		"a name that leaves":    {Name: "../outside", Typeflag: tar.TypeReg, Mode: 0o600},
		"a name that is rooted": {Name: "/etc/outside", Typeflag: tar.TypeReg, Mode: 0o600},
		"a symbolic link":       {Name: "work/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stream bytes.Buffer
			archive := tar.NewWriter(&stream)
			if err := archive.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			err := volumes.Unpack(&stream, t.TempDir())
			if err == nil {
				t.Fatalf("Unpack() accepted %s", name)
			}
			if !strings.Contains(err.Error(), "leaves") && !strings.Contains(err.Error(), "neither") {
				t.Fatalf("Unpack() = %v", err)
			}
		})
	}
}
