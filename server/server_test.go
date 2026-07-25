package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type blockingEngine struct {
	started chan struct{}
	release chan struct{}
}

func (e *blockingEngine) Run() error {
	return nil
}

func (e *blockingEngine) ServeHTTP(writer http.ResponseWriter, _ *http.Request) {
	close(e.started)
	<-e.release
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("done"))
}

func TestHTTPServerTimeoutConfiguration(t *testing.T) {
	server, err := New("127.0.0.1:0")
	require.NoError(t, err)
	httpServer := server.newHTTPServer()

	require.Equal(t, 10*time.Second, httpServer.ReadHeaderTimeout)
	require.Equal(t, 120*time.Second, httpServer.IdleTimeout)
	require.Zero(t, httpServer.ReadTimeout)
	require.Zero(t, httpServer.WriteTimeout)
	require.Equal(t, 1<<20, httpServer.MaxHeaderBytes)
}

func TestRunReturnsCleanlyWhenContextCancelled(t *testing.T) {
	server, err := New("127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx)
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down after context cancellation")
	}
}

func TestGracefulShutdownCompletesInflightRequestAndClosesListener(t *testing.T) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	engine := &blockingEngine{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	runningServer := &Server{
		bind:   listener.Addr().String(),
		engine: engine,
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- runningServer.serve(ctx, listener)
	}()

	responseDone := make(chan error, 1)
	go func() {
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			"http://"+listener.Addr().String(),
			nil,
		)
		if err != nil {
			responseDone <- err
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			responseDone <- err
			return
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err == nil && string(raw) != "done" {
			err = io.ErrUnexpectedEOF
		}
		responseDone <- err
	}()

	select {
	case <-engine.started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for {
		dialer := &net.Dialer{Timeout: 20 * time.Millisecond}
		connection, dialErr := dialer.DialContext(t.Context(), "tcp", listener.Addr().String())
		if dialErr != nil {
			break
		}
		_ = connection.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepts new connections during shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(engine.release)
	require.NoError(t, <-responseDone)
	require.NoError(t, <-runDone)
}
