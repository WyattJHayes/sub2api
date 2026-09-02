package repository

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type scannerArtifactStoreStub struct {
	body []byte
}

func (s *scannerArtifactStoreStub) PresignPut(context.Context, service.ArtifactObjectPutRequest, time.Duration) (*service.ArtifactObjectUpload, error) {
	return nil, fmt.Errorf("unexpected presign")
}

func (s *scannerArtifactStoreStub) Head(context.Context, string) (*service.ArtifactObjectMetadata, error) {
	return nil, fmt.Errorf("unexpected head")
}

func (s *scannerArtifactStoreStub) PresignGet(context.Context, string, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, fmt.Errorf("unexpected presign get")
}

func (s *scannerArtifactStoreStub) Delete(context.Context, string) error {
	return fmt.Errorf("unexpected delete")
}

func (s *scannerArtifactStoreStub) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(s.body))), nil
}

func TestClamAVArtifactScannerStreamsObjectAndAcceptsCleanResult(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		buffer := make([]byte, len("zINSTREAM\x00"))
		if _, err := io.ReadFull(conn, buffer); err != nil {
			serverErr <- err
			return
		}
		if string(buffer) != "zINSTREAM\x00" {
			serverErr <- fmt.Errorf("unexpected ClamAV command %q", buffer)
			return
		}
		var received strings.Builder
		for {
			lengthBytes := make([]byte, 4)
			if _, err := io.ReadFull(conn, lengthBytes); err != nil {
				serverErr <- err
				return
			}
			length := int(lengthBytes[0])<<24 | int(lengthBytes[1])<<16 | int(lengthBytes[2])<<8 | int(lengthBytes[3])
			if length == 0 {
				break
			}
			chunk := make([]byte, length)
			if _, err := io.ReadFull(conn, chunk); err != nil {
				serverErr <- err
				return
			}
			_, _ = received.Write(chunk)
		}
		if received.String() != "trusted evidence" {
			serverErr <- fmt.Errorf("unexpected object body %q", received.String())
			return
		}
		_, _ = io.WriteString(conn, "stream: OK\x00")
		serverErr <- nil
	}()

	store := &scannerArtifactStoreStub{body: []byte("trusted evidence")}
	scanner, err := NewClamAVArtifactScanner(store, listener.Addr().String(), time.Second)
	require.NoError(t, err)

	result, err := scanner.Scan(context.Background(), "run/sample/evidence.json", service.ArtifactObjectMetadata{
		ObjectKey: "run/sample/evidence.json",
		Bytes:     int64(len(store.body)),
		MIMEType:  "application/json",
		SHA256:    "1ed1d397965e1052a9b4505c38f7c25d6629ad86b3570488e2ff1ad07913f802",
	})
	require.NoError(t, err)
	require.Equal(t, service.ArtifactScanClean, result.Status)
	require.Equal(t, "clamav", result.Scanner)
	require.NoError(t, <-serverErr)
}

func TestClamAVArtifactScannerRejectsBodyHashMismatchAfterCleanScan(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		command := make([]byte, len(clamAVCommand))
		if _, err := io.ReadFull(conn, command); err != nil {
			return
		}
		for {
			lengthBytes := make([]byte, 4)
			if _, err := io.ReadFull(conn, lengthBytes); err != nil {
				return
			}
			length := int(lengthBytes[0])<<24 | int(lengthBytes[1])<<16 | int(lengthBytes[2])<<8 | int(lengthBytes[3])
			if length == 0 {
				break
			}
			if _, err := io.CopyN(io.Discard, conn, int64(length)); err != nil {
				return
			}
		}
		_, _ = io.WriteString(conn, "stream: OK\n")
	}()

	store := &scannerArtifactStoreStub{body: []byte("tampered evidence")}
	scanner, err := NewClamAVArtifactScanner(store, listener.Addr().String(), time.Second)
	require.NoError(t, err)

	result, err := scanner.Scan(context.Background(), "run/sample/evidence.json", service.ArtifactObjectMetadata{
		ObjectKey: "run/sample/evidence.json",
		Bytes:     int64(len(store.body)),
		MIMEType:  "application/json",
		SHA256:    testArtifactSHA256,
	})
	require.ErrorIs(t, err, service.ErrArtifactObjectMismatch)
	require.Equal(t, service.ArtifactScanFailed, result.Status)
}

func TestClamAVArtifactScannerRejectsInfectedResult(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		command := make([]byte, len("zINSTREAM\x00"))
		if _, err := io.ReadFull(conn, command); err != nil {
			return
		}
		for {
			lengthBytes := make([]byte, 4)
			if _, err := io.ReadFull(conn, lengthBytes); err != nil {
				return
			}
			length := int(lengthBytes[0])<<24 | int(lengthBytes[1])<<16 | int(lengthBytes[2])<<8 | int(lengthBytes[3])
			if length == 0 {
				break
			}
			chunk := make([]byte, length)
			if _, err := io.ReadFull(conn, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(conn, "stream: Eicar-Test-Signature FOUND\x00")
	}()

	store := &scannerArtifactStoreStub{body: []byte("infected")}
	scanner, err := NewClamAVArtifactScanner(store, listener.Addr().String(), time.Second)
	require.NoError(t, err)

	result, err := scanner.Scan(context.Background(), "run/sample/evidence.json", service.ArtifactObjectMetadata{ObjectKey: "run/sample/evidence.json", Bytes: 8, SHA256: "c810e76f2125db71bfbdd7e29ce902f37f5b2250c48c16d241bd46c70aed1a91"})
	require.NoError(t, err)
	require.Equal(t, service.ArtifactScanRejected, result.Status)
	require.Contains(t, result.Reason, "Eicar-Test-Signature")
}
