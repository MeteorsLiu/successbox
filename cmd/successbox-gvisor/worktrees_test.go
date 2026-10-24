package main

import (
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestHideWorktreeState(t *testing.T) {
	base := t.TempDir()
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "state")
	image := filepath.Join(base, "image")
	alias := filepath.Join(base, "alias")
	for _, p := range []string{state, image, filepath.Join(state, "private")} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, root, state string
		binds             []specs.Mount
		want              []string
	}{
		{name: "host root", root: "/", state: state, want: []string{state}},
		{name: "separate image", root: image, state: state},
		{name: "bind alias", root: "/", state: state, binds: []specs.Mount{{Type: "bind", Source: alias, Destination: "/user"}}, want: []string{state, "/user/state"}},
		{name: "private subdirectory", root: image, state: state, binds: []specs.Mount{{Type: "bind", Source: filepath.Join(state, "private"), Destination: "/user"}}, want: []string{"/user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &specs.Spec{Root: &specs.Root{Path: tc.root}, Mounts: append([]specs.Mount(nil), tc.binds...)}
			if err := hideWorktreeState(spec, tc.state); err != nil {
				t.Fatal(err)
			}
			added := spec.Mounts[len(tc.binds):]
			if len(added) != len(tc.want) {
				t.Fatalf("mounts=%+v", added)
			}
			for i, m := range added {
				if m.Destination != tc.want[i] || m.Type != "tmpfs" {
					t.Fatalf("mount=%+v, want %q", m, tc.want[i])
				}
			}
		})
	}
}
