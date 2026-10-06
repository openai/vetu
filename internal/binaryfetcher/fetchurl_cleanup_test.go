package binaryfetcher_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cirruslabs/vetu/internal/binaryfetcher"
	"github.com/stretchr/testify/require"
)

type observedBody struct {
	io.ReadCloser
	closed      *atomic.Int32
	readStarted chan struct{}
}

func (body *observedBody) Read(data []byte) (int, error) {
	count, err := body.ReadCloser.Read(data)
	if count > 0 && body.readStarted != nil {
		close(body.readStarted)
		body.readStarted = nil
	}
	return count, err
}

func (body *observedBody) Close() error {
	body.closed.Add(1)
	return body.ReadCloser.Close()
}

type observingTransport struct {
	inner       http.RoundTripper
	closed      *atomic.Int32
	readStarted chan struct{}
}

func (transport observingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.inner.RoundTrip(request)
	if err == nil {
		response.Body = &observedBody{ReadCloser: response.Body, closed: transport.closed, readStarted: transport.readStarted}
	}
	return response, err
}

func TestFailedDownloadsReleaseOwnedArtifacts(t *testing.T) {
	for _, scenario := range []string{"complete", "empty", "truncated", "cancelled", "http-error"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("TMPDIR", directory)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch scenario {
				case "complete":
					_, _ = io.WriteString(writer, "Synthetic binary data")
				case "empty":
					writer.WriteHeader(http.StatusOK)
				case "http-error":
					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(writer, "Synthetic server failure")
				default:
					writer.Header().Set("Content-Length", "1000")
					_, _ = io.WriteString(writer, "Synthetic partial data")
					if scenario == "cancelled" {
						writer.(http.Flusher).Flush()
						<-request.Context().Done()
					}
				}
			}))
			defer server.Close()
			defer server.CloseClientConnections()
			previous := http.DefaultTransport
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			var closed atomic.Int32
			var readStarted chan struct{}
			if scenario == "cancelled" {
				readStarted = started
			}
			http.DefaultTransport = observingTransport{inner: transport, closed: &closed, readStarted: readStarted}
			defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections() }()
			if scenario == "cancelled" {
				go func() { <-started; cancel() }()
			}
			path, err := binaryfetcher.FetchURLToFile(ctx, server.URL)
			entries, readErr := os.ReadDir(directory)
			require.NoError(t, readErr)
			if scenario == "complete" || scenario == "empty" {
				require.NoError(t, err)
				require.Len(t, entries, 1)
				require.Equal(t, directory, filepath.Dir(path))
				data, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				if scenario == "complete" {
					require.Equal(t, "Synthetic binary data", string(data))
				} else {
					require.Empty(t, data)
				}
			} else {
				require.Error(t, err)
				require.Empty(t, path)
				require.Empty(t, entries, "failed transfers must not leave partial download files")
			}
			require.EqualValues(t, 1, closed.Load(), "every received response body must be closed")
		})
	}
}

func TestSuccessfulFetchLeavesBodyOwnershipWithCaller(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "Synthetic response")
	}))
	defer server.Close()
	response, err := binaryfetcher.FetchURL(t.Context(), server.URL)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "Synthetic response", string(data))
}
