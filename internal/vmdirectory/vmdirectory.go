package vmdirectory

import (
	"encoding/json"
	"fmt"
	"github.com/cirruslabs/vetu/internal/filelock"
	"github.com/cirruslabs/vetu/internal/pidlock"
	"github.com/cirruslabs/vetu/internal/vmconfig"
	"io/fs"
	"os"
	"path/filepath"
)

type VMDirectory struct {
	baseDir string
}

type State string

const (
	StateStopped State = "stopped"
	StateRunning State = "running"
)

func Load(path string) (*VMDirectory, error) {
	vmDir := &VMDirectory{
		baseDir: path,
	}

	return vmDir, nil
}

func (vmDir *VMDirectory) FileLock(lockType filelock.LockType) (*filelock.FileLock, error) {
	return filelock.New(vmDir.Path(), lockType)
}

func (vmDir *VMDirectory) PIDLock() (*pidlock.PIDLock, error) {
	return pidlock.New(vmDir.ConfigPath())
}

func (vmDir *VMDirectory) Path() string {
	return vmDir.baseDir
}

func (vmDir *VMDirectory) Size() (uint64, error) {
	var result uint64

	if err := filepath.WalkDir(vmDir.Path(), func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fileInfo, err := dirEntry.Info()
		if err != nil {
			return err
		}

		result += uint64(fileInfo.Size())

		return nil
	}); err != nil {
		return 0, err
	}

	return result, nil
}

func (vmDir *VMDirectory) Running() bool {
	lock, err := pidlock.New(vmDir.ConfigPath())
	if err != nil {
		return false
	}
	defer lock.Close()

	pid, err := lock.Pid()
	if err != nil {
		return false
	}

	return pid != 0
}

func (vmDir *VMDirectory) State() State {
	if vmDir.Running() {
		return StateRunning
	} else {
		return StateStopped
	}
}

func (vmDir *VMDirectory) ConfigPath() string {
	return filepath.Join(vmDir.baseDir, "config.json")
}

func (vmDir *VMDirectory) KernelPath() string {
	return filepath.Join(vmDir.baseDir, "kernel")
}

func (vmDir *VMDirectory) InitramfsPath() string {
	return filepath.Join(vmDir.baseDir, "initramfs")
}

func (vmDir *VMDirectory) Config() (*vmconfig.VMConfig, error) {
	vmConfigBytes, err := os.ReadFile(vmDir.ConfigPath())
	if err != nil {
		return nil, fmt.Errorf("failed to read VM's config: %v", err)
	}

	vmConfig, err := vmconfig.NewFromJSON(vmConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse VM's config: %v", err)
	}

	return vmConfig, nil
}

func (vmDir *VMDirectory) SetConfig(vmConfig *vmconfig.VMConfig) error {
	vmConfigJSONBytes, err := json.Marshal(vmConfig)
	if err != nil {
		return fmt.Errorf("failed to serialize VM's config: %v", err)
	}

	if err := os.WriteFile(vmDir.ConfigPath(), vmConfigJSONBytes, 0600); err != nil {
		return fmt.Errorf("failed to write VM's config: %v", err)
	}

	return nil
}
