//go:build linux && (amd64 || arm64)

package main

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPathnameArguments(t *testing.T) {
	args := [6]uint64{7, 123, 9, 456}
	for _, tc := range []struct {
		name string
		want []pathArgument
	}{
		{"open", []pathArgument{{unix.AT_FDCWD, 0}}},
		{"openat", []pathArgument{{7, 1}}},
		{"openat2", []pathArgument{{7, 1}}},
		{"fstatat", []pathArgument{{7, 1}}},
		{"newfstatat", []pathArgument{{7, 1}}},
		{"renameat2", []pathArgument{{7, 1}, {9, 3}}},
		{"linkat", []pathArgument{{7, 1}, {9, 3}}},
		{"symlinkat", []pathArgument{{123, 2}}},
		{"symlink", []pathArgument{{unix.AT_FDCWD, 1}}},
		{"write", nil},
		{"mmap", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathnameArguments(tc.name, args); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMappingsRespectMounts(t *testing.T) {
	m := &manager{root: "/", mappings: map[string]string{"/repo": "/private/repo"}, mounts: []specs.Mount{
		{Destination: "/repo/readonly", Type: "bind", Source: "/source", Options: []string{"ro"}},
		{Destination: "/repo/temporary", Type: "tmpfs"},
		{Destination: "/repo/other", Type: "bind", Source: "/other"},
	}}
	for _, tc := range []struct{ path, want string }{
		{"/repo/file", "/private/repo/file"}, {"/repo/readonly/file", ""}, {"/repo/temporary/file", ""}, {"/repo/other/file", ""}, {"/repository/file", ""},
	} {
		if got := m.mapped(tc.path); got != tc.want {
			t.Errorf("mapped(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
