package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func startShutdownTestServer(t *testing.T, handler http.Handler) (*http.Server, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{Handler: handler}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case err := <-serveDone:
			require.True(t, errors.Is(err, http.ErrServerClosed), "Serve() error = %v", err)
		case <-time.After(time.Second):
			t.Error("test HTTP server did not stop")
		}
	})
	return server, "http://" + listener.Addr().String()
}

func TestShutdownHTTPServerReturnsNilAfterActiveRequestFinishes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server, baseURL := startShutdownTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	response := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Get(baseURL)
		if err == nil {
			response <- resp
			return
		}
		response <- nil
	}()
	<-started
	close(release)

	require.NoError(t, shutdownHTTPServer(server, 5*time.Second))
	resp := <-response
	if resp != nil {
		_ = resp.Body.Close()
	}
}

func TestShutdownHTTPServerReturnsDeadlineExceededForBlockedRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server, baseURL := startShutdownTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	response := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Get(baseURL)
		if err == nil {
			response <- resp
			return
		}
		response <- nil
	}()
	<-started

	err := shutdownHTTPServer(server, 20*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(release)
	resp := <-response
	if resp != nil {
		_ = resp.Body.Close()
	}
}
