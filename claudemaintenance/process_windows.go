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

const maxOwnedProcesses = 4096

var isProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

type ownedWindowsProcess struct {
	handle windows.Handle
	stop   bool
}

type ownedWindowsJob struct {
	mu        sync.Mutex
	handle    windows.Handle
	processes map[uint32]*ownedWindowsProcess
	uncertain bool
}

// capture pins process identities before termination. A PID obtained from the
// job may have exited before OpenProcess, so verify membership on the handle.
func (j *ownedWindowsJob) capture() (uint32, error) {
	var list struct {
		assigned, count uint32
		pids            [maxOwnedProcesses]uintptr
	}
	if err := windows.QueryInformationJobObject(j.handle, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&list)), uint32(unsafe.Sizeof(list)), nil); err != nil || list.count > maxOwnedProcesses || list.assigned != list.count {
		return 0, errProcessDrain
	}
	for _, value := range list.pids[:list.count] {
		if value == 0 || value > uintptr(^uint32(0)) {
			return 0, errProcessDrain
		}
		pid := uint32(value)
		if _, known := j.processes[pid]; known {
			continue
		}
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			// The process object no longer exists; its PID cannot identify
			// an unacknowledged live process from this snapshot.
			continue
		}
		if err != nil {
			return 0, errProcessDrain
		}
		var member uint32
		ok, _, _ := isProcessInJob.Call(uintptr(handle), uintptr(j.handle), uintptr(unsafe.Pointer(&member)))
		if ok == 0 {
			windows.CloseHandle(handle)
			return 0, errProcessDrain
		}
		if member == 0 {
			// A reused PID must never authorize terminating a foreign process.
			windows.CloseHandle(handle)
			continue
		}
		if len(j.processes) == maxOwnedProcesses {
			windows.CloseHandle(handle)
			return 0, errProcessDrain
		}
		j.processes[pid] = &ownedWindowsProcess{handle: handle}
	}
	return list.count, nil
}

func (j *ownedWindowsJob) terminate() error {
	for _, process := range j.processes {
		if process.stop {
			continue
		}
		state, err := windows.WaitForSingleObject(process.handle, 0)
		if err != nil {
			return errProcessDrain
		}
		if state == windows.WAIT_OBJECT_0 {
			process.stop = true
			continue
		}
		if state != uint32(windows.WAIT_TIMEOUT) {
			return errProcessDrain
		}
		err = windows.TerminateProcess(process.handle, 1)
		// An independently exiting process can deny a repeated termination.
		// Neither this result nor job accounting is an exit acknowledgement.
		if err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return errProcessDrain
		}
		process.stop = true
	}
	return nil
}

func waitOwnedProcess(handle windows.Handle, deadline time.Time) error {
	var milliseconds uint32
	if remaining := time.Until(deadline); remaining > 0 {
		wait := remaining / time.Millisecond
		if remaining%time.Millisecond != 0 {
			wait++
		}
		milliseconds = uint32(min(wait, time.Duration(windows.INFINITE-1)))
	}
	state, err := windows.WaitForSingleObject(handle, milliseconds)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		return errProcessDrain
	}
	return nil
}

func (j *ownedWindowsJob) stop() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return nil
	}
	_, err := j.capture()
	if err == nil {
		err = j.terminate()
	}
	if err != nil {
		j.uncertain = true
		_ = windows.TerminateJobObject(j.handle, 1)
		return errProcessDrain
	}
	return nil
}

func (j *ownedWindowsJob) cleanup(deadline time.Time) (result error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	defer func() {
		if result != nil {
			// Best-effort containment on an unconfirmed stop. Never call this
			// before capturing handles on a path that can report success.
			_ = windows.TerminateJobObject(j.handle, 1)
		}
		for _, process := range j.processes {
			windows.CloseHandle(process.handle)
		}
		windows.CloseHandle(j.handle)
		j.handle = 0
	}()
	if j.uncertain {
		return errProcessDrain
	}
	for {
		count, err := j.capture()
		if err != nil {
			return errProcessDrain
		}
		if err = j.terminate(); err != nil {
			return errProcessDrain
		}
		if count == 0 {
			// TerminateJobObject can empty its PID/accounting lists before
			// process handles are signaled. Keep every captured handle until
			// the real exit acknowledgement, within the original deadline.
			for _, process := range j.processes {
				if err = waitOwnedProcess(process.handle, deadline); err != nil {
					return err
				}
			}
			return nil
		}
		if !time.Now().Before(deadline) {
			return errProcessDrain
		}
		// A child can spawn between snapshot and termination. Re-enumerate
		// the owned job rather than treating the first snapshot as complete.
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
}

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
	owned := &ownedWindowsJob{handle: job, processes: make(map[uint32]*ownedWindowsProcess)}
	// Start suspended: no credential access or generation is possible before
	// the process belongs to our kill-on-close job. Failure is not a fallback.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW}
	start := func(ctx context.Context) error {
		owned.mu.Lock()
		defer owned.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
		if err != nil {
			return err
		}
		if err = windows.AssignProcessToJobObject(job, process); err != nil {
			windows.CloseHandle(process)
			return err
		}
		// Cmd.Wait closes its process handle. Retain our own for cleanup.
		owned.processes[uint32(cmd.Process.Pid)] = &ownedWindowsProcess{handle: process}
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
	finish := func(deadline time.Time) error {
		once.Do(func() { finishErr = owned.cleanup(deadline) })
		return finishErr
	}
	return start, owned.stop, finish, nil
}
