//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MeteorsLiu/successbox/runtime/internal/intercept"
	"golang.org/x/sys/unix"
)

func TestCallbackSharedMemory(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "partial"}[partial], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			address := &net.UnixAddr{Name: filepath.Join(t.TempDir(), "callback.sock"), Net: "unixpacket"}
			listener, err := net.ListenUnix("unixpacket", address)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			var borrowed *syscallEvent
			go func() {
				done <- serve(ctx, listener, func(call *syscallEvent) error {
					borrowed = call
					data, err := call.mmap(0, 8193)
					if partial && !errors.Is(err, unix.EFAULT) {
						return errors.New("partial copy lost errno")
					}
					if !partial && err != nil {
						return err
					}
					want := 8193
					if partial {
						want = 17
					}
					if data.Addr != 0x100000 || len(data.Data) != want {
						return errors.New("incorrect guest memory view")
					}
					data.Data[0] = 'a'
					data.Data[want-1] = 'z'
					call.Args[1] = data.Addr
					return nil
				})
			}()
			connection, err := net.DialUnix("unixpacket", nil, address)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			event, _ := json.Marshal(intercept.Message{Op: "syscall", Name: "openat"})
			if _, err := connection.Write(event); err != nil {
				t.Fatal(err)
			}
			buffer := make([]byte, 65536)
			n, err := connection.Read(buffer)
			if err != nil {
				t.Fatal(err)
			}
			var request intercept.Message
			if err := json.Unmarshal(buffer[:n], &request); err != nil || request.Op != "mmap" {
				t.Fatalf("request: %+v, %v", request, err)
			}
			file, err := os.CreateTemp(t.TempDir(), "pages")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			length := 3 * unix.Getpagesize()
			if err := file.Truncate(int64(length)); err != nil {
				t.Fatal(err)
			}
			size := uint64(8193)
			var errno uint32
			if partial {
				size = 17
				errno = uint32(unix.EFAULT)
			}
			response, _ := json.Marshal(intercept.Message{Address: 0x100000, Size: size, Length: uint64(length), Errno: errno, Ranges: []intercept.Range{
				{FD: 0, Start: 0, Length: uint64(length)},
			}})
			if _, _, err := connection.WriteMsgUnix(response, unix.UnixRights(int(file.Fd())), nil); err != nil {
				t.Fatal(err)
			}
			n, err = connection.Read(buffer)
			if err != nil {
				t.Fatal(err)
			}
			var resume intercept.Message
			if err := json.Unmarshal(buffer[:n], &resume); err != nil || resume.Op != "resume" || resume.Args[1] != 0x100000 || resume.Error != "" {
				t.Fatalf("resume %+v: %v", resume, err)
			}
			contents := make([]byte, size)
			if _, err := file.ReadAt(contents, 0); err != nil {
				t.Fatal(err)
			}
			if contents[0] != 'a' || contents[len(contents)-1] != 'z' {
				t.Fatal("callback copied data instead of sharing pages")
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("serve shutdown: %v", err)
			}
			if borrowed.active || len(borrowed.mappings) != 0 {
				t.Fatal("callback retained its aliases")
			}
			if _, err := borrowed.readString(1); err == nil {
				t.Fatal("expired callback was usable")
			}
		})
	}
}

func TestCallbackListenerClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := &net.UnixAddr{Name: filepath.Join(t.TempDir(), "callback.sock"), Net: "unixpacket"}
	listener, err := net.ListenUnix("unixpacket", address)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, listener, func(*syscallEvent) error { return nil }) }()
	peer, err := net.DialUnix("unixpacket", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	listener.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unexpected listener close was hidden")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listener close left a connection blocked")
	}
}
