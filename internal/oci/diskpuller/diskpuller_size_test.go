package diskpuller_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/cirruslabs/vetu/internal/oci/diskpuller"
	"github.com/cirruslabs/vetu/internal/vmdirectory"
	"github.com/pierrec/lz4/v4"
	"github.com/regclient/regclient"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/ref"
	"github.com/stretchr/testify/require"
)

func TestPullDisksValidatesDecompressedSize(t *testing.T) {
	for _, test := range []struct {
		name  string
		data  []byte
		size  int64
		valid bool
	}{
		{name: "exact", data: []byte("contents"), size: 8, valid: true},
		{name: "short", data: []byte("contents"), size: 16},
		{name: "long", data: []byte("contents"), size: 4},
		{name: "empty", data: nil, size: 0, valid: true},
		{name: "missing", data: nil, size: 8},
		{name: "nonempty_zero_size", data: []byte("contents"), size: 0},
		{name: "sparse", data: make([]byte, 65536), size: 65536, valid: true},
		{name: "sparse_short", data: make([]byte, 65536), size: 65537},
		{name: "sparse_long", data: make([]byte, 65536), size: 65535},
	} {
		t.Run(test.name, func(t *testing.T) {
			var compressed bytes.Buffer
			writer := lz4.NewWriter(&compressed)
			_, err := writer.Write(test.data)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			ctx := context.Background()
			client := regclient.New()
			reference, err := ref.New("ocidir://" + filepath.Join(t.TempDir(), "registry") + ":test")
			require.NoError(t, err)
			disk, err := client.BlobPut(ctx, reference, descriptor.Descriptor{}, bytes.NewReader(compressed.Bytes()))
			require.NoError(t, err)
			disk.Annotations = map[string]string{"test.uncompressed-size": strconv.FormatInt(test.size, 10)}
			vmDir, err := vmdirectory.Load(t.TempDir())
			require.NoError(t, err)

			err = diskpuller.PullDisks(ctx, client, reference, vmDir, 1, []descriptor.Descriptor{disk},
				func(descriptor.Descriptor) (string, error) { return "disk.img", nil },
				"test.uncompressed-size", func(reader io.Reader) io.Reader { return lz4.NewReader(reader) })
			if !test.valid {
				require.Error(t, err, "a mismatched disk layer must not be accepted")
				info, statErr := os.Stat(filepath.Join(vmDir.Path(), "disk.img"))
				require.NoError(t, statErr)
				require.Equal(t, test.size, info.Size(), "oversized layers must not write beyond their assigned range")
				return
			}
			require.NoError(t, err)
			actual, err := os.ReadFile(filepath.Join(vmDir.Path(), "disk.img"))
			require.NoError(t, err)
			require.Equal(t, string(test.data), string(actual))
		})
	}
}

func TestPullDisksCombinesParallelLayers(t *testing.T) {
	ctx := context.Background()
	client := regclient.New()
	reference, err := ref.New("ocidir://" + filepath.Join(t.TempDir(), "registry") + ":test")
	require.NoError(t, err)
	parts := [][]byte{[]byte("first"), make([]byte, 65536), []byte("last")}
	var disks []descriptor.Descriptor
	for _, part := range parts {
		var compressed bytes.Buffer
		writer := lz4.NewWriter(&compressed)
		_, err := writer.Write(part)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		disk, err := client.BlobPut(ctx, reference, descriptor.Descriptor{}, bytes.NewReader(compressed.Bytes()))
		require.NoError(t, err)
		disk.Annotations = map[string]string{"test.uncompressed-size": strconv.Itoa(len(part))}
		disks = append(disks, disk)
	}
	vmDir, err := vmdirectory.Load(t.TempDir())
	require.NoError(t, err)
	err = diskpuller.PullDisks(ctx, client, reference, vmDir, 3, disks,
		func(descriptor.Descriptor) (string, error) { return "disk.img", nil },
		"test.uncompressed-size", func(reader io.Reader) io.Reader { return lz4.NewReader(reader) })
	require.NoError(t, err)
	actual, err := os.ReadFile(filepath.Join(vmDir.Path(), "disk.img"))
	require.NoError(t, err)
	require.Equal(t, bytes.Join(parts, nil), actual)
}

func TestPullDisksChecksTheFinalChecksum(t *testing.T) {
	var compressed bytes.Buffer
	writer := lz4.NewWriter(&compressed)
	_, err := writer.Write([]byte("contents"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	payload := compressed.Bytes()
	payload[len(payload)-1] ^= 0xff

	ctx := context.Background()
	client := regclient.New()
	reference, err := ref.New("ocidir://" + filepath.Join(t.TempDir(), "registry") + ":test")
	require.NoError(t, err)
	disk, err := client.BlobPut(ctx, reference, descriptor.Descriptor{}, bytes.NewReader(payload))
	require.NoError(t, err)
	disk.Annotations = map[string]string{"test.uncompressed-size": "8"}
	vmDir, err := vmdirectory.Load(t.TempDir())
	require.NoError(t, err)
	err = diskpuller.PullDisks(ctx, client, reference, vmDir, 1, []descriptor.Descriptor{disk},
		func(descriptor.Descriptor) (string, error) { return "disk.img", nil },
		"test.uncompressed-size", func(reader io.Reader) io.Reader { return lz4.NewReader(reader) })
	require.Error(t, err, "size validation must still read and validate the stream checksum")
}
