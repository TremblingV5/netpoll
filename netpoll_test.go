// Copyright 2022 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !windows
// +build !windows

package netpoll

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func MustNil(t *testing.T, val interface{}) {
	t.Helper()
	Assert(t, val == nil, val)
	if val != nil {
		t.Fatal("assertion nil failed, val=", val)
	}
}

func MustTrue(t *testing.T, cond bool) {
	t.Helper()
	if !cond {
		t.Fatal("assertion true failed.")
	}
}

func Equal(t *testing.T, got, expect interface{}) {
	t.Helper()
	if got != expect {
		t.Fatalf("assertion equal failed, got=[%v], expect=[%v]", got, expect)
	}
}

func Assert(t *testing.T, cond bool, val ...interface{}) {
	t.Helper()
	if !cond {
		if len(val) > 0 {
			val = append([]interface{}{"assertion failed:"}, val...)
			t.Fatal(val...)
		} else {
			t.Fatal("assertion failed")
		}
	}
}

func TestEqual(t *testing.T) {
	var err error
	MustNil(t, err)
	MustTrue(t, err == nil)
	Equal(t, err, nil)
	Assert(t, err == nil, err)
}

func TestOnConnect(t *testing.T) {
	var network, address = "tcp", ":8888"
	req, resp := "ping", "pong"
	var loop = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			return nil
		},
		WithOnConnect(func(ctx context.Context, conn Connection) context.Context {
			for {
				input, err := conn.Reader().Next(len(req))
				if errors.Is(err, ErrEOF) || errors.Is(err, ErrConnClosed) {
					return ctx
				}
				MustNil(t, err)
				Equal(t, string(input), req)

				_, err = conn.Writer().WriteString(resp)
				MustNil(t, err)
				err = conn.Writer().Flush()
				MustNil(t, err)
			}
		}),
	)
	var conn, err = DialConnection(network, address, time.Second)
	MustNil(t, err)

	for i := 0; i < 1024; i++ {
		_, err = conn.Writer().WriteString(req)
		MustNil(t, err)
		err = conn.Writer().Flush()
		MustNil(t, err)

		input, err := conn.Reader().Next(len(resp))
		MustNil(t, err)
		Equal(t, string(input), resp)
	}

	err = conn.Close()
	MustNil(t, err)

	err = loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestOnConnectWrite(t *testing.T) {
	var network, address = "tcp", ":8888"
	var loop = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			return nil
		},
		WithOnConnect(func(ctx context.Context, connection Connection) context.Context {
			_, err := connection.Write([]byte("hello"))
			MustNil(t, err)
			return ctx
		}),
	)
	var conn, err = DialConnection(network, address, time.Second)
	MustNil(t, err)
	s, err := conn.Reader().ReadString(5)
	MustNil(t, err)
	MustTrue(t, s == "hello")

	err = loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestOnDisconnect(t *testing.T) {
	type ctxKey struct{}
	var network, address = "tcp", ":8888"
	var canceled, closed int32
	var conns int32 = 100
	req := "ping"
	var loop = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			cancelFunc, _ := ctx.Value(ctxKey{}).(context.CancelFunc)
			MustTrue(t, cancelFunc != nil)
			Assert(t, ctx.Done() != nil)

			buf, err := connection.Reader().Next(4) // should consumed all data
			MustNil(t, err)
			Equal(t, string(buf), req)
			select {
			case <-ctx.Done():
				atomic.AddInt32(&canceled, 1)
			case <-time.After(time.Second):
			}
			return nil
		},
		WithOnConnect(func(ctx context.Context, conn Connection) context.Context {
			conn.AddCloseCallback(func(connection Connection) error {
				atomic.AddInt32(&closed, 1)
				return nil
			})
			ctx, cancel := context.WithCancel(ctx)
			return context.WithValue(ctx, ctxKey{}, cancel)
		}),
		WithOnDisconnect(func(ctx context.Context, conn Connection) {
			cancelFunc, _ := ctx.Value(ctxKey{}).(context.CancelFunc)
			MustTrue(t, cancelFunc != nil)
			cancelFunc()
		}),
	)

	for i := int32(0); i < conns; i++ {
		var conn, err = DialConnection(network, address, time.Second)
		MustNil(t, err)

		_, err = conn.Writer().WriteString(req)
		MustNil(t, err)
		err = conn.Writer().Flush()
		MustNil(t, err)

		err = conn.Close()
		MustNil(t, err)
	}
	for atomic.LoadInt32(&closed) < conns {
		t.Logf("closed: %d, canceled: %d", atomic.LoadInt32(&closed), atomic.LoadInt32(&canceled))
		runtime.Gosched()
	}
	Equal(t, atomic.LoadInt32(&closed), conns)
	Equal(t, atomic.LoadInt32(&canceled), conns)

	err := loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestOnDisconnectWhenOnConnect(t *testing.T) {
	type ctxPrepareKey struct{}
	type ctxConnectKey struct{}
	var network, address = "tcp", ":8888"
	var conns int32 = 100
	var wg sync.WaitGroup
	wg.Add(int(conns) * 3)
	var loop = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			_, _ = connection.Reader().Next(connection.Reader().Len())
			return nil
		},
		WithOnPrepare(func(connection Connection) context.Context {
			defer wg.Done()
			var counter int32
			return context.WithValue(context.Background(), ctxPrepareKey{}, &counter)
		}),
		WithOnConnect(func(ctx context.Context, conn Connection) context.Context {
			defer wg.Done()
			t.Logf("OnConnect: %v", conn.RemoteAddr())
			time.Sleep(time.Millisecond * 10) // wait for closed called
			counter := ctx.Value(ctxPrepareKey{}).(*int32)
			ok := atomic.CompareAndSwapInt32(counter, 0, 1)
			Assert(t, ok)
			return context.WithValue(ctx, ctxConnectKey{}, "123")
		}),
		WithOnDisconnect(func(ctx context.Context, conn Connection) {
			defer wg.Done()
			t.Logf("OnDisconnect: %v", conn.RemoteAddr())
			counter, _ := ctx.Value(ctxPrepareKey{}).(*int32)
			ok := atomic.CompareAndSwapInt32(counter, 1, 2)
			Assert(t, ok)
			v := ctx.Value(ctxConnectKey{}).(string)
			Equal(t, v, "123")
		}),
	)

	for i := int32(0); i < conns; i++ {
		var conn, err = DialConnection(network, address, time.Second)
		MustNil(t, err)
		err = conn.Close()
		t.Logf("Close: %v", conn.LocalAddr())
		MustNil(t, err)
	}

	wg.Wait()
	err := loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestGracefulExit(t *testing.T) {
	var network, address = "tcp", ":8888"

	// exit without processing connections
	var eventLoop1 = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			return nil
		})
	var _, err = DialConnection(network, address, time.Second)
	MustNil(t, err)
	err = eventLoop1.Shutdown(context.Background())
	MustNil(t, err)

	// exit with processing connections
	var eventLoop2 = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			time.Sleep(10 * time.Second)
			return nil
		})
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			var conn, err = DialConnection(network, address, time.Second)
			MustNil(t, err)
			_, err = conn.Write(make([]byte, 16))
			MustNil(t, err)
		}
	}
	var ctx2, cancel2 = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	err = eventLoop2.Shutdown(ctx2)
	MustTrue(t, err != nil)
	Equal(t, err.Error(), ctx2.Err().Error())

	// exit with some processing connections
	var eventLoop3 = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			time.Sleep(time.Duration(rand.Intn(3)) * time.Second)
			if l := connection.Reader().Len(); l > 0 {
				var _, err = connection.Reader().Next(l)
				MustNil(t, err)
			}
			return nil
		})
	for i := 0; i < 10; i++ {
		var conn, err = DialConnection(network, address, time.Second)
		MustNil(t, err)
		if i%2 == 0 {
			_, err = conn.Write(make([]byte, 16))
			MustNil(t, err)
		}
	}
	var ctx3, cancel3 = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel3()
	err = eventLoop3.Shutdown(ctx3)
	MustNil(t, err)
}

func TestCloseCallbackWhenOnRequest(t *testing.T) {
	var network, address = "tcp", ":8888"
	var requested, closed = make(chan struct{}), make(chan struct{})
	var loop = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			_, err := connection.Reader().Next(connection.Reader().Len())
			MustNil(t, err)
			err = connection.AddCloseCallback(func(connection Connection) error {
				closed <- struct{}{}
				return nil
			})
			MustNil(t, err)
			requested <- struct{}{}
			return nil
		},
	)
	var conn, err = DialConnection(network, address, time.Second)
	MustNil(t, err)
	_, err = conn.Writer().WriteString("hello")
	MustNil(t, err)
	err = conn.Writer().Flush()
	MustNil(t, err)
	<-requested
	err = conn.Close()
	MustNil(t, err)
	<-closed

	err = loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestCloseCallbackWhenOnConnect(t *testing.T) {
	var network, address = "tcp", ":8888"
	var connected, closed = make(chan struct{}), make(chan struct{})
	var loop = newTestEventLoop(network, address,
		nil,
		WithOnConnect(func(ctx context.Context, connection Connection) context.Context {
			err := connection.AddCloseCallback(func(connection Connection) error {
				closed <- struct{}{}
				return nil
			})
			MustNil(t, err)
			connected <- struct{}{}
			return ctx
		}),
	)
	var conn, err = DialConnection(network, address, time.Second)
	MustNil(t, err)
	err = conn.Close()
	MustNil(t, err)

	<-connected
	<-closed

	err = loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestCloseConnWhenOnConnect(t *testing.T) {
	var network, address = "tcp", ":8888"
	conns := 10
	var wg sync.WaitGroup
	wg.Add(conns)
	var loop = newTestEventLoop(network, address,
		nil,
		WithOnConnect(func(ctx context.Context, connection Connection) context.Context {
			defer wg.Done()
			err := connection.Close()
			MustNil(t, err)
			return ctx
		}),
	)

	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var conn, err = DialConnection(network, address, time.Second)
			if err != nil {
				return
			}
			_, err = conn.Reader().Next(1)
			Assert(t, errors.Is(err, ErrEOF))
			err = conn.Close()
			MustNil(t, err)
		}()
	}

	wg.Wait()
	err := loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestServerReadAndClose(t *testing.T) {
	var network, address = "tcp", ":18888"
	var sendMsg = []byte("hello")
	var closed int32
	var loop = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			_, err := connection.Reader().Next(len(sendMsg))
			MustNil(t, err)

			err = connection.Close()
			MustNil(t, err)
			atomic.AddInt32(&closed, 1)
			return nil
		},
	)

	var conn, err = DialConnection(network, address, time.Second)
	MustNil(t, err)
	_, err = conn.Writer().WriteBinary(sendMsg)
	MustNil(t, err)
	err = conn.Writer().Flush()
	MustNil(t, err)

	for atomic.LoadInt32(&closed) == 0 {
		runtime.Gosched() // wait for poller close connection
	}
	time.Sleep(time.Millisecond * 50)
	_, err = conn.Writer().WriteBinary(sendMsg)
	MustNil(t, err)
	err = conn.Writer().Flush()
	MustTrue(t, errors.Is(err, ErrConnClosed))

	err = loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestServerPanicAndClose(t *testing.T) {
	var network, address = "tcp", ":18888"
	var sendMsg = []byte("hello")
	var paniced int32
	var loop = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			_, err := connection.Reader().Next(len(sendMsg))
			MustNil(t, err)
			atomic.StoreInt32(&paniced, 1)
			panic("test")
		},
	)

	var conn, err = DialConnection(network, address, time.Second)
	MustNil(t, err)
	_, err = conn.Writer().WriteBinary(sendMsg)
	MustNil(t, err)
	err = conn.Writer().Flush()
	MustNil(t, err)

	for atomic.LoadInt32(&paniced) == 0 {
		runtime.Gosched() // wait for poller close connection
	}
	for conn.IsActive() {
		runtime.Gosched() // wait for poller close connection
	}

	err = loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestClientWriteAndClose(t *testing.T) {
	var (
		network, address            = "tcp", ":18889"
		connnum                     = 10
		packetsize, packetnum       = 1000 * 5, 1
		recvbytes             int32 = 0
	)
	var loop = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			buf, err := connection.Reader().Next(connection.Reader().Len())
			if errors.Is(err, ErrConnClosed) {
				return err
			}
			MustNil(t, err)
			atomic.AddInt32(&recvbytes, int32(len(buf)))
			return nil
		},
	)
	var wg sync.WaitGroup
	for i := 0; i < connnum; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var conn, err = DialConnection(network, address, time.Second)
			MustNil(t, err)
			sendMsg := make([]byte, packetsize)
			for j := 0; j < packetnum; j++ {
				_, err = conn.Write(sendMsg)
				MustNil(t, err)
			}
			err = conn.Close()
			MustNil(t, err)
		}()
	}
	wg.Wait()
	exceptbytes := int32(packetsize * packetnum * connnum)
	for atomic.LoadInt32(&recvbytes) != exceptbytes {
		t.Logf("left %d bytes not received", exceptbytes-atomic.LoadInt32(&recvbytes))
		runtime.Gosched()
	}
	err := loop.Shutdown(context.Background())
	MustNil(t, err)
}

func TestServerAcceptWhenTooManyOpenFiles(t *testing.T) {
	if os.Getenv("N_LOCAL") == "" {
		t.Skip("Only test for debug purpose")
		return
	}

	var originalRlimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &originalRlimit)
	MustNil(t, err)
	t.Logf("Original RLimit: %v", originalRlimit)

	rlimit := syscall.Rlimit{Cur: 32, Max: originalRlimit.Max}
	err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlimit)
	MustNil(t, err)
	err = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit)
	MustNil(t, err)
	t.Logf("New RLimit: %v", rlimit)
	defer func() { // reset
		err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &originalRlimit)
		MustNil(t, err)
	}()

	var network, address = "tcp", ":18888"
	var connected int32
	var loop = newTestEventLoop(network, address,
		func(ctx context.Context, connection Connection) error {
			buf, err := connection.Reader().Next(connection.Reader().Len())
			connection.Writer().WriteBinary(buf)
			connection.Writer().Flush()
			return err
		},
		WithOnConnect(func(ctx context.Context, connection Connection) context.Context {
			atomic.AddInt32(&connected, 1)
			t.Logf("Conn[%s] accepted", connection.RemoteAddr())
			return ctx
		}),
		WithOnDisconnect(func(ctx context.Context, connection Connection) {
			t.Logf("Conn[%s] disconnected", connection.RemoteAddr())
		}),
	)
	time.Sleep(time.Millisecond * 10)

	// out of fds
	files := make([]*os.File, 0)
	for {
		f, err := os.Open("/dev/null")
		if err != nil {
			Assert(t, isOutOfFdErr(errors.Unwrap(err)), err)
			break
		}
		files = append(files, f)
	}
	go func() {
		time.Sleep(time.Second * 10)
		t.Logf("close all files")
		for _, f := range files {
			f.Close()
		}
	}()

	// we should use telnet manually
	var connections = 1
	for atomic.LoadInt32(&connected) < int32(connections) {
		t.Logf("connected=%d", atomic.LoadInt32(&connected))
		time.Sleep(time.Second)
	}
	time.Sleep(time.Second * 10)

	err = loop.Shutdown(context.Background())
	MustNil(t, err)
}

func createTestListener(network, address string) (Listener, error) {
	for {
		ln, err := CreateListener(network, address)
		if err == nil {
			return ln, nil
		}
		time.Sleep(time.Millisecond * 100)
	}
}

func newTestEventLoop(network, address string, onRequest OnRequest, opts ...Option) EventLoop {
	ln, err := createTestListener(network, address)
	if err != nil {
		panic(err)
	}
	elp, err := NewEventLoop(onRequest, opts...)
	if err != nil {
		panic(err)
	}
	go elp.Serve(ln)
	return elp
}
