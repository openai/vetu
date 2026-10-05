package diskpuller_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cirruslabs/vetu/internal/oci/diskpuller"
	"github.com/cirruslabs/vetu/internal/vmdirectory"
	"github.com/regclient/regclient"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/ref"
	"github.com/stretchr/testify/require"
)

const cancellationSizeAnnotation = "test.uncompressed-size"

func TestPullDisksRejectsAlreadyCancelledContext(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(strconv.FormatBool(expired), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			want := context.Canceled
			if expired {
				ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
				defer cancel()
				want = context.DeadlineExceeded
			}
			vmDir, err := vmdirectory.Load(t.TempDir())
			require.NoError(t, err)
			path := filepath.Join(vmDir.Path(), "disk.img")
			require.NoError(t, os.WriteFile(path, []byte("original"), 0600))
			disk := descriptor.Descriptor{Annotations: map[string]string{cancellationSizeAnnotation: "4"}}
			err = diskpuller.PullDisks(ctx, nil, ref.Ref{}, vmDir, 1, []descriptor.Descriptor{disk},
				func(descriptor.Descriptor) (string, error) { return "disk.img", nil },
				cancellationSizeAnnotation, func(reader io.Reader) io.Reader { return reader })
			require.ErrorIs(t, err, want)
			actual, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "original", string(actual))
		})
	}
}

func TestPullDisksReportsCancellationBeforeScheduling(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	vmDir, err := vmdirectory.Load(t.TempDir())
	require.NoError(t, err)
	disk := descriptor.Descriptor{Annotations: map[string]string{cancellationSizeAnnotation: "4"}}
	err = diskpuller.PullDisks(ctx, nil, ref.Ref{}, vmDir, 1, []descriptor.Descriptor{disk},
		func(descriptor.Descriptor) (string, error) {
			cancel()
			return "disk.img", nil
		}, cancellationSizeAnnotation, func(reader io.Reader) io.Reader { return reader })
	require.ErrorIs(t, err, context.Canceled)
}

type cancelOnEOF struct {
	io.Reader
	cancel context.CancelFunc
}

func (reader cancelOnEOF) Read(buffer []byte) (int, error) {
	n, err := reader.Reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		reader.cancel()
	}
	return n, err
}

func TestPullDisksCancellationAfterLastLayerRead(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			data := []byte("synthetic disk contents")
			client := regclient.New()
			reference, err := ref.New("ocidir://" + filepath.Join(t.TempDir(), "registry") + ":fixture")
			require.NoError(t, err)
			disk, err := client.BlobPut(ctx, reference, descriptor.Descriptor{}, bytes.NewReader(data))
			require.NoError(t, err)
			disk.Annotations = map[string]string{cancellationSizeAnnotation: strconv.Itoa(len(data))}
			vmDir, err := vmdirectory.Load(t.TempDir())
			require.NoError(t, err)
			err = diskpuller.PullDisks(ctx, client, reference, vmDir, 1, []descriptor.Descriptor{disk},
				func(descriptor.Descriptor) (string, error) { return "disk.img", nil },
				cancellationSizeAnnotation, func(reader io.Reader) io.Reader {
					if cancelled {
						return cancelOnEOF{Reader: reader, cancel: cancel}
					}
					return reader
				})
			if cancelled {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			actual, err := os.ReadFile(filepath.Join(vmDir.Path(), "disk.img"))
			require.NoError(t, err)
			require.Equal(t, data, actual)
		})
	}
}
