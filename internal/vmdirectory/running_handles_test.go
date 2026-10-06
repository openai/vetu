package vmdirectory_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cirruslabs/vetu/internal/pidlock"
	"github.com/cirruslabs/vetu/internal/vmdirectory"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const statusLockEnvironment = "VETU_TEST_STATUS_LOCK"

func TestStatusQueryClosesConfigDescriptors(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(strconv.FormatBool(running), func(t *testing.T) {
			directory := t.TempDir()
			config := filepath.Join(directory, "config.json")
			require.NoError(t, os.WriteFile(config, []byte("{}"), 0o600))
			vm, err := vmdirectory.Load(directory)
			require.NoError(t, err)
			// Clean only descriptors referencing this fixture if the regression fails.
			t.Cleanup(func() {
				for _, descriptor := range configDescriptors(t, config) {
					_ = unix.Close(descriptor)
				}
			})
			if running {
				startStatusLockHolder(t, config)
			}
			require.Empty(t, configDescriptors(t, config))
			for i := 0; i < 32; i++ {
				require.Equal(t, running, vm.Running())
				want := vmdirectory.StateStopped
				if running {
					want = vmdirectory.StateRunning
				}
				require.Equal(t, want, vm.State())
			}
			require.Empty(t, configDescriptors(t, config), "status polling must not retain config handles")
		})
	}
}

func TestStatusQueryMissingConfigRemainsStopped(t *testing.T) {
	vm, err := vmdirectory.Load(t.TempDir())
	require.NoError(t, err)
	require.False(t, vm.Running())
	require.Equal(t, vmdirectory.StateStopped, vm.State())
}

func configDescriptors(t *testing.T, path string) []int {
	t.Helper()
	var info unix.Stat_t
	require.NoError(t, unix.Stat(path, &info))
	fdDirectory := "/proc/self/fd"
	if _, err := os.Stat(fdDirectory); err != nil {
		fdDirectory = "/dev/fd"
	}
	entries, err := os.ReadDir(fdDirectory)
	require.NoError(t, err)
	var result []int
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		require.NoError(t, err)
		var candidate unix.Stat_t
		if unix.Fstat(fd, &candidate) == nil && candidate.Dev == info.Dev && candidate.Ino == info.Ino {
			result = append(result, fd)
		}
	}
	return result
}

func startStatusLockHolder(t *testing.T, path string) {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestStatusLockHelper$")
	command.Env = append(os.Environ(), statusLockEnvironment+"="+path)
	stdin, err := command.StdinPipe()
	require.NoError(t, err)
	stdout, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		require.NoError(t, command.Wait())
		cancel()
	})
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", ready)
}

func TestStatusLockHelper(t *testing.T) {
	path := os.Getenv(statusLockEnvironment)
	if path == "" {
		return
	}
	lock, err := pidlock.New(path)
	require.NoError(t, err)
	defer lock.Close()
	require.NoError(t, lock.Trylock())
	fmt.Println("ready")
	_, err = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)
}
