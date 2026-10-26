//go:build windows

package health

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var getDiskFreeSpaceEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// freeSpace reports the bytes available to the calling user at path.
func freeSpace(path string) (uint64, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available, total, totalFree uint64
	// #nosec G103 -- GetDiskFreeSpaceExW requires pointers into this function's
	// own stack frame; nothing is shared across goroutines.
	result, _, callErr := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(pointer)),
		uintptr(unsafe.Pointer(&available)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if result == 0 {
		return 0, callErr
	}
	return available, nil
}
