package diskpuller

import (
	"context"
	"fmt"

	"github.com/cirruslabs/vetu/internal/progresshelper"
	"github.com/cirruslabs/vetu/internal/sparseio"
	"github.com/cirruslabs/vetu/internal/vmdirectory"
	"github.com/dustin/go-humanize"
	"github.com/regclient/regclient"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/ref"
	"github.com/samber/lo"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/sync/errgroup"

	"io"
	"os"
	"path/filepath"
	"strconv"
)

type NameFromDiskDescriptorFunc func(diskDescriptor descriptor.Descriptor) (string, error)
type InitializeDecompressorFunc func(compressedReader io.Reader) io.Reader

type diskTask struct {
	Desc             descriptor.Descriptor
	Path             string
	Offset           int64
	UncompressedSize int64
}

func PullDisks(
	ctx context.Context,
	client *regclient.RegClient,
	reference ref.Ref,
	vmDir *vmdirectory.VMDirectory,
	concurrency int,
	disks []descriptor.Descriptor,
	nameFromDiskDescriptor NameFromDiskDescriptorFunc,
	uncompressedSizeAnnotation string,
	initializeDecompressor InitializeDecompressorFunc,
) error {
	// Process VM's disks by converting them into
	// disk tasks for further parallel processing
	var diskTasks []*diskTask
	diskNameToOffset := map[string]int64{}

	for _, disk := range disks {
		// Extract name
		diskName, err := nameFromDiskDescriptor(disk)
		if err != nil {
			return err
		}

		// Extract and parse uncompressed size
		uncompressedSizeRaw, ok := disk.Annotations[uncompressedSizeAnnotation]
		if !ok {
			return fmt.Errorf("disk layer has no %s annotation", uncompressedSizeAnnotation)
		}
		uncompressedSize, err := strconv.ParseInt(uncompressedSizeRaw, 10, 64)
		if err != nil {
			return err
		}

		diskTasks = append(diskTasks, &diskTask{
			Desc:             disk,
			Path:             filepath.Join(vmDir.Path(), diskName),
			Offset:           diskNameToOffset[diskName],
			UncompressedSize: uncompressedSize,
		})

		diskNameToOffset[diskName] += uncompressedSize
	}

	// Pre-create and truncate disk files
	for diskName, offset := range diskNameToOffset {
		diskFile, err := os.OpenFile(filepath.Join(vmDir.Path(), diskName), os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			return err
		}

		if err := diskFile.Truncate(offset); err != nil {
			return err
		}

		if err := diskFile.Close(); err != nil {
			return err
		}
	}

	// Indicate that we're started pulling and show the progress bar
	totalUncompressedDisksSizeBytes := lo.Sum(lo.Values(diskNameToOffset))
	totalCompressedDisksSizeBytes := lo.Sum(lo.Map(disks, func(diskDesc descriptor.Descriptor, index int) int64 {
		return diskDesc.Size
	}))
	fmt.Printf("pulling %d disk(s) (%s compressed, %s uncompressed)...\n", len(diskNameToOffset),
		humanize.Bytes(uint64(totalCompressedDisksSizeBytes)),
		humanize.Bytes(uint64(totalUncompressedDisksSizeBytes)))

	var progressBar *progressbar.ProgressBar

	if totalCompressedDisksSizeBytes > 0 {
		progressBar = progresshelper.DefaultBytes(totalCompressedDisksSizeBytes)
	} else {
		progressBar = progresshelper.DefaultBytesSilent(-1)
	}

	// Process disk tasks with the specified concurrency
	diskTasksGroup, diskTasksCtx := errgroup.WithContext(ctx)

	diskTasksGroup.SetLimit(concurrency)

	for _, diskTask := range diskTasks {
		if diskTasksCtx.Err() != nil {
			break
		}

		diskTasksGroup.Go(func() error {
			return diskTask.process(diskTasksCtx, client, reference, progressBar, initializeDecompressor)
		})
	}

	// Wait for the disk tasks to finish
	diskTasksErr := diskTasksGroup.Wait()

	// Since we've finished with pulling disks,
	// we can finish the associated progress bar
	finishErr := progressBar.Finish()

	// Prefer diskTasksErr over finishErr
	if diskTasksErr != nil {
		return diskTasksErr
	}

	return finishErr
}

func (diskTask *diskTask) process(
	ctx context.Context,
	client *regclient.RegClient,
	reference ref.Ref,
	progressBar *progressbar.ProgressBar,
	initializeDecompressor InitializeDecompressorFunc,
) error {
	// Open disk file and seek to the specified offset
	diskFile, err := os.OpenFile(diskTask.Path, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}

	defer diskFile.Close()

	diskFileAtOffset := io.NewOffsetWriter(diskFile, diskTask.Offset)

	// Pull disk layer from the OCI registry
	blobReader, err := client.BlobGet(ctx, reference, diskTask.Desc)
	if err != nil {
		return err
	}
	defer blobReader.Close()

	// Decompress the disk data on-the-fly and write it to the disk file
	progressBarReader := progressbar.NewReader(blobReader, progressBar)
	decompressor := initializeDecompressor(&progressBarReader)

	// Keep every layer within its assigned range, including sparse zero blocks.
	limited := &io.LimitedReader{R: decompressor, N: diskTask.UncompressedSize}
	if err := sparseio.Copy(diskFileAtOffset, limited); err != nil {
		return err
	}
	if limited.N != 0 {
		return fmt.Errorf("%w: disk layer %s decoded %d bytes, expected %d", io.ErrUnexpectedEOF,
			diskTask.Desc.Digest, diskTask.UncompressedSize-limited.N, diskTask.UncompressedSize)
	}

	// Read through the decompressor's end marker/checksum without writing beyond
	// this layer. One additional decoded byte means the declared size is wrong.
	var extra [1]byte
	if n, err := io.ReadFull(decompressor, extra[:]); n != 0 {
		return fmt.Errorf("disk layer %s exceeds its declared uncompressed size of %d bytes",
			diskTask.Desc.Digest, diskTask.UncompressedSize)
	} else if err != io.EOF {
		return err
	}

	return diskFile.Close()
}
