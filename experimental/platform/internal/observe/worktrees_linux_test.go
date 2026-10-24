//go:build linux && (amd64 || arm64)

package observe

import (
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/arch"
)

func TestPathnameArguments(t *testing.T) {
	args := arch.SyscallArguments{{Value: 7}, {Value: 123}, {Value: 9}, {Value: 456}}
	for _, tc := range []struct {
		name string
		want []pathArgument
	}{
		{"open", []pathArgument{{unix.AT_FDCWD, 0}}},
		{"openat", []pathArgument{{7, 1}}},
		{"openat2", []pathArgument{{7, 1}}},
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
