package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/MeteorsLiu/successbox/runtime/internal/intercept"
	"golang.org/x/sys/unix"
)

// serve delivers syscall callbacks on private unixpacket connections. Cancel
// ctx to close the listener and its connections; callbacks must not outlive it.
func serve(ctx context.Context, listener *net.UnixListener, handler func(*syscallEvent) error) error {
	var mu sync.Mutex
	peers := make(map[*net.UnixConn]struct{})
	var workers sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for conn := range peers {
			conn.Close()
		}
	})
	defer stop()
	failures := make(chan error, 1)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() == nil {
				select {
				case failures <- err:
				default:
				}
				cancel()
			}
			break
		}
		mu.Lock()
		if ctx.Err() != nil {
			mu.Unlock()
			conn.Close()
			break
		}
		peers[conn] = struct{}{}
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer conn.Close()
			defer func() { mu.Lock(); delete(peers, conn); mu.Unlock() }()
			buffer := make([]byte, 65536)
			for {
				n, _, flags, _, err := conn.ReadMsgUnix(buffer, nil)
				if err == nil && n == 0 {
					err = io.EOF
				}
				if err == nil && flags&unix.MSG_TRUNC != 0 {
					err = errors.New("truncated syscall event")
				}
				if err != nil {
					if !errors.Is(err, io.EOF) && ctx.Err() == nil {
						select {
						case failures <- err:
						default:
						}
						cancel()
					}
					return
				}
				var event intercept.Message
				if err := json.Unmarshal(buffer[:n], &event); err != nil || event.Op != "syscall" {
					select {
					case failures <- errors.New("invalid syscall event"):
					default:
					}
					cancel()
					return
				}
				call := &syscallEvent{uid: event.UID, gid: event.GID, groups: event.Groups, Name: event.Name, Args: event.Args, First: event.First, conn: conn, active: true}
				callbackErr := handler(call)
				if err := call.finish(callbackErr); err != nil {
					if ctx.Err() == nil {
						select {
						case failures <- err:
						default:
						}
						cancel()
					}
					return
				}
			}
		}()
	}
	workers.Wait()
	select {
	case err := <-failures:
		return err
	default:
		return ctx.Err()
	}
}
