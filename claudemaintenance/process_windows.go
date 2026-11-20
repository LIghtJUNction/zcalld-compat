//go:build windows

package claudemaintenance

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepareProcess(cmd *exec.Cmd) (func(context.Context) error, func() error, func(time.Time) error, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return nil, nil, nil, err
	}
	// Start suspended: no credential access or generation is possible before
	// the process belongs to our kill-on-close job. Failure is not a fallback.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW}
	stop := func() error { return windows.TerminateJobObject(job, 1) }
	cleanup := func(deadline time.Time) error {
		defer windows.CloseHandle(job)
		if err := windows.TerminateJobObject(job, 1); err != nil {
			return errProcessDrain
		}
		// TerminateJobObject starts termination; it does not wait for owned
		// descendants. Parent Wait alone is also insufficient.
		for {
			var accounting struct {
				UserTime, KernelTime, PeriodUserTime, PeriodKernelTime           int64
				PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
			}
			if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
				return errProcessDrain
			}
			if accounting.ActiveProcesses == 0 {
				return nil
			}
			if !time.Now().Before(deadline) {
				return errProcessDrain
			}
			time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
		}
	}
	start := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		if err != nil {
			return err
		}
		defer windows.CloseHandle(process)
		if err = windows.AssignProcessToJobObject(job, process); err != nil {
			return err
		}
		snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(snapshot)
		entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
		var thread uint32
		for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
			if entry.OwnerProcessID == uint32(cmd.Process.Pid) {
				if thread != 0 {
					return errors.New("Claude suspended process has unexpected threads")
				}
				thread = entry.ThreadID
			}
			entry.Size = uint32(unsafe.Sizeof(entry))
		}
		if thread == 0 {
			return errors.New("Claude suspended process thread is unavailable")
		}
		handle, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, thread)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(handle)
		if err = ctx.Err(); err != nil {
			return err
		}
		_, err = windows.ResumeThread(handle)
		return err
	}
	var once sync.Once
	var finishErr error
	finish := func(deadline time.Time) error { once.Do(func() { finishErr = cleanup(deadline) }); return finishErr }
	return start, stop, finish, nil
}
