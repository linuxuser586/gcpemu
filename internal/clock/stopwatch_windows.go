package clock

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	qpc      = kernel32.NewProc("QueryPerformanceCounter")
	qpf      = kernel32.NewProc("QueryPerformanceFrequency")
	freq     = perfFrequency()
)

// perfFrequency is QueryPerformanceFrequency, which never fails on
// Windows XP and later.
func perfFrequency() int64 {
	var f int64
	syscall.SyscallN(qpf.Addr(), uintptr(unsafe.Pointer(&f)))
	return f
}

// ticks is QueryPerformanceCounter in nanoseconds.
func ticks() int64 {
	var c int64
	syscall.SyscallN(qpc.Addr(), uintptr(unsafe.Pointer(&c)))
	return c/freq*1e9 + c%freq*1e9/freq
}
