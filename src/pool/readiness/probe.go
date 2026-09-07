package readiness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	mountutils "k8s.io/mount-utils"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

type Check struct {
	OK      bool
	Known   bool
	Reason  string
	Message string
}

type Result struct {
	Mounted          Check
	Writable         Check
	CapacityReadable Check
}

type Inspector interface {
	Inspect(volumeapi.Pool) Result
}

type Probe struct {
	HostRoot     string
	isMountPoint func(string) (bool, error)
	write        func(string) error
	statFS       func(string) error
}

func NewProbe(hostRoot string) *Probe {
	mounter := mountutils.New("")
	return &Probe{
		HostRoot:     hostRoot,
		isMountPoint: mounter.IsMountPoint,
		write:        writeProbe,
		statFS: func(path string) error {
			return syscall.Statfs(path, &syscall.Statfs_t{})
		},
	}
}

func (p *Probe) Inspect(pool volumeapi.Pool) Result {
	path, err := hostPath(p.HostRoot, pool.MountPath)
	if err != nil {
		failed := failure(err)
		return Result{Mounted: failed, Writable: skipped(), CapacityReadable: skipped()}
	}
	mounted, err := p.isMountPoint(path)
	if err != nil {
		failed := failure(err)
		return Result{Mounted: failed, Writable: skipped(), CapacityReadable: skipped()}
	}
	if !mounted {
		return Result{
			Mounted:          Check{Known: true, Reason: "NotMounted", Message: fmt.Sprintf("%s is not a mount point", pool.MountPath)},
			Writable:         skipped(),
			CapacityReadable: skipped(),
		}
	}
	result := Result{
		Mounted: Check{OK: true, Known: true, Reason: "Mounted", Message: fmt.Sprintf("%s is a mount point", pool.MountPath)},
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
	case errors.Is(err, syscall.EROFS):
		reason = "ReadOnly"
	case errors.Is(err, syscall.ENOSPC):
		reason = "NoSpace"
	}
	return Check{Known: true, Reason: reason, Message: err.Error()}
}

func skipped() Check {
	return Check{Reason: "ProbeSkipped", Message: "check skipped because the mount prerequisite failed"}
}
