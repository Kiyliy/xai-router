//go:build windows

package diskusage

import (
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

func of(path string) (Usage, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return Usage{}, err
	}
	var freeToCaller, total, free uint64
	r, _, e := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return Usage{}, e
	}
	return Usage{Total: total, Free: freeToCaller}, nil
}
