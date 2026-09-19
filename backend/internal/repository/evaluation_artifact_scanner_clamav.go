package repository

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	clamAVCommand      = "zINSTREAM\x00"
	clamAVChunkSize    = 32 * 1024
	clamAVMaxReplySize = 4096
)

type ClamAVArtifactScanner struct {
	store   service.EvaluationArtifactObjectStore
	address string
	timeout time.Duration
	dialer  net.Dialer
}

var _ service.ArtifactScanner = (*ClamAVArtifactScanner)(nil)

func NewClamAVArtifactScanner(store service.EvaluationArtifactObjectStore, address string, timeout time.Duration) (*ClamAVArtifactScanner, error) {
	if store == nil || strings.TrimSpace(address) == "" {
		return nil, service.ErrArtifactObjectStoreUnavailable
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &ClamAVArtifactScanner{store: store, address: strings.TrimSpace(address), timeout: timeout}, nil
}

func (s *ClamAVArtifactScanner) Scan(ctx context.Context, objectKey string, metadata service.ArtifactObjectMetadata) (service.ArtifactScanResult, error) {
	started := time.Now().UTC()
	result := service.ArtifactScanResult{Scanner: "clamav", ScannedAt: started}
	if s == nil || s.store == nil || strings.TrimSpace(s.address) == "" {
		result.Status = service.ArtifactScanFailed
		result.Reason = "scanner is not configured"
		return result, service.ErrArtifactObjectStoreUnavailable
	}
	scanCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	body, err := s.store.Open(scanCtx, objectKey)
	if err != nil {
		result.Status = service.ArtifactScanFailed
		result.Reason = "open artifact object: " + err.Error()
		return result, err
	}
	defer func() { _ = body.Close() }()
	conn, err := s.dialer.DialContext(scanCtx, "tcp", s.address)
	if err != nil {
		result.Status = service.ArtifactScanFailed
		result.Reason = "connect to ClamAV: " + err.Error()
		return result, err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := scanCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, clamAVCommand); err != nil {
		result.Status = service.ArtifactScanFailed
		result.Reason = "write ClamAV command: " + err.Error()
		return result, err
	}
	buffer := make([]byte, clamAVChunkSize)
	digest := sha256.New()
	var total int64
	for {
		read, readErr := body.Read(buffer)
		if read > 0 {
			if uint64(read) > uint64(^uint32(0)) {
				result.Status = service.ArtifactScanFailed
				result.Reason = "artifact scan chunk is too large"
				return result, fmt.Errorf("artifact scan chunk is too large")
			}
			var length [4]byte
			binary.BigEndian.PutUint32(length[:], uint32(read))
			if _, err := conn.Write(length[:]); err != nil {
				result.Status = service.ArtifactScanFailed
				result.Reason = "write ClamAV chunk length: " + err.Error()
				return result, err
			}
			if _, err := conn.Write(buffer[:read]); err != nil {
				result.Status = service.ArtifactScanFailed
				result.Reason = "write ClamAV chunk: " + err.Error()
				return result, err
			}
			if _, err := digest.Write(buffer[:read]); err != nil {
				result.Status = service.ArtifactScanFailed
				result.Reason = "hash artifact object: " + err.Error()
				return result, err
			}
			total += int64(read)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			result.Status = service.ArtifactScanFailed
			result.Reason = "read artifact object: " + readErr.Error()
			return result, readErr
		}
	}
	var terminator [4]byte
	if _, err := conn.Write(terminator[:]); err != nil {
		result.Status = service.ArtifactScanFailed
		result.Reason = "finish ClamAV stream: " + err.Error()
		return result, err
	}
	if metadata.Bytes >= 0 && total != metadata.Bytes {
		result.Status = service.ArtifactScanFailed
		result.Reason = fmt.Sprintf("scanned byte count %d does not match metadata %d", total, metadata.Bytes)
		return result, service.ErrArtifactObjectMismatch
	}
	actualSHA256 := hex.EncodeToString(digest.Sum(nil))
	if actualSHA256 != strings.TrimSpace(metadata.SHA256) {
		result.Status = service.ArtifactScanFailed
		result.Reason = fmt.Sprintf("scanned SHA256 %s does not match metadata", actualSHA256)
		return result, service.ErrArtifactObjectMismatch
	}
	response, err := readClamAVResponse(conn)
	if err != nil {
		result.Status = service.ArtifactScanFailed
		result.Reason = "read ClamAV response: " + err.Error()
		return result, err
	}
	response = strings.TrimSpace(response)
	switch {
	case strings.HasSuffix(response, "OK"):
		result.Status = service.ArtifactScanClean
		result.Reason = response
		return result, nil
	case strings.Contains(response, "FOUND"):
		result.Status = service.ArtifactScanRejected
		result.Reason = response
		return result, nil
	default:
		result.Status = service.ArtifactScanFailed
		result.Reason = response
		return result, fmt.Errorf("ClamAV scan failed: %s", response)
	}
}

func readClamAVResponse(conn net.Conn) (string, error) {
	reader := bufio.NewReader(conn)
	response := make([]byte, 0, 64)
	for len(response) < clamAVMaxReplySize {
		value, err := reader.ReadByte()
		if err == io.EOF {
			if len(response) > 0 {
				return strings.TrimSpace(string(response)), nil
			}
			return "", err
		}
		if err != nil {
			return "", err
		}
		if value == 0 || value == '\n' {
			return strings.TrimSpace(string(response)), nil
		}
		response = append(response, value)
	}
	return "", fmt.Errorf("ClamAV response exceeds %d bytes", clamAVMaxReplySize)
}
