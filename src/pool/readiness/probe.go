package readiness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

type Check struct {
	OK      bool
	Known   bool
	Reason  string
	Message string
}

type Result struct {
	Accessible       Check
	Writable         Check
	CapacityReadable Check
}

type Inspector interface {
	Inspect(volumeapi.Pool) Result
}

type Probe struct {
	HostRoot string
	inspect  func(string) error
	write    func(string) error
	statFS   func(string) error
}

func NewProbe(hostRoot string) *Probe {
	return &Probe{
		HostRoot: hostRoot,
		inspect:  inspectDirectory,
		write:    writeProbe,
		statFS: func(path string) error {
			return syscall.Statfs(path, &syscall.Statfs_t{})
		},
	}
}

func (p *Probe) Inspect(pool volumeapi.Pool) Result {
	path, err := hostPath(p.HostRoot, pool.MountPath)
	if err != nil {
		failed := failure(err)
		return Result{Accessible: failed, Writable: skipped(), CapacityReadable: skipped()}
	}
	if err := p.inspect(path); err != nil {
		failed := failure(err)
		return Result{Accessible: failed, Writable: skipped(), CapacityReadable: skipped()}
	}
	result := Result{
		Accessible: Check{OK: true, Known: true, Reason: "DirectoryAccessible", Message: fmt.Sprintf("%s is an accessible directory", pool.MountPath)},
	}
	if err := p.write(path); err != nil {
		result.Writable = failure(err)
	} else {
		result.Writable = Check{OK: true, Known: true, Reason: "Writable", Message: "temporary directory, write, sync, and cleanup succeeded"}
	}
	if err := p.statFS(path); err != nil {
		result.CapacityReadable = failure(err)
	} else {
		result.CapacityReadable = Check{OK: true, Known: true, Reason: "CapacityReadable", Message: "filesystem capacity is readable"}
	}
	return result
}

func inspectDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: %w", path, syscall.ENOTDIR)
	}
	return nil
}

func hostPath(hostRoot, mountPath string) (string, error) {
	hostRoot = filepath.Clean(hostRoot)
	mountPath = filepath.Clean(mountPath)
	if !filepath.IsAbs(hostRoot) || !filepath.IsAbs(mountPath) || mountPath == string(filepath.Separator) {
		return "", fmt.Errorf("host root and Pool mount path must be absolute non-root paths")
	}
	path := filepath.Join(hostRoot, strings.TrimPrefix(mountPath, string(filepath.Separator)))
	relative, err := filepath.Rel(hostRoot, path)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("Pool mount path %q escapes host root %q", mountPath, hostRoot)
	}
	return path, nil
}

func writeProbe(root string) (result error) {
	directory, err := os.MkdirTemp(root, ".shiftpv-probe-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(directory); result == nil && err != nil {
			result = err
		}
	}()
	file, err := os.OpenFile(filepath.Join(directory, "writable"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write([]byte("shiftpv pool readiness\n")); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func failure(err error) Check {
	reason := "ProbeFailed"
	switch {
	case errors.Is(err, fs.ErrNotExist):
		reason = "PathMissing"
	case errors.Is(err, fs.ErrPermission):
		reason = "PermissionDenied"
	case errors.Is(err, syscall.ENOTDIR):
		reason = "NotDirectory"
	case errors.Is(err, syscall.EROFS):
		reason = "ReadOnly"
	case errors.Is(err, syscall.ENOSPC):
		reason = "NoSpace"
	}
	return Check{Known: true, Reason: reason, Message: err.Error()}
}

func skipped() Check {
	return Check{Reason: "ProbeSkipped", Message: "check skipped because the directory prerequisite failed"}
}
