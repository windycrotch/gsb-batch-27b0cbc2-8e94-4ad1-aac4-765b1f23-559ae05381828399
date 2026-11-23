package oss

import (
	"syscall"
	"unsafe"
)

const (
	iocNrbits   = 8
	iocTypebits = 8
	iocNrshift  = 0

	iocTypeshift = iocNrshift + iocNrbits
	iocSizeshift = iocTypeshift + iocTypebits
	iocDirshift  = iocSizeshift + iocSizebits
	iocSizemask  = (1 << iocSizebits) - 1
)

const (
	iocIn    = iocWrite << iocDirshift
	iocOut   = iocRead << iocDirshift
	iocInout = iocIn | iocOut
)

func _io(x, y int) uint {
	return (iocNone << iocDirshift) | (uint(x) << 8) | uint(y)
}

func _ior(x, y int) uint {
	var t int32

	return iocOut | ((uint(unsafe.Sizeof(t)) & iocSizemask) << 16) | (uint(x) << 8) | uint(y)
}

func _iow(x, y int) uint {
	var t int32

	return iocIn | ((uint(unsafe.Sizeof(t)) & iocSizemask) << 16) | (uint(x) << 8) | uint(y)
}

func _iowr(x, y int) uint {
	var t int32

	return iocInout | ((uint(unsafe.Sizeof(t)) & iocSizemask) << 16) | (uint(x) << 8) | uint(y)
}

func _iori(x, y int) uint {
	var t audioBufInfo

	return iocOut | ((uint(unsafe.Sizeof(t)) & iocSizemask) << 16) | (uint(x) << 8) | uint(y)
}

func ioctl(fd uintptr, req uint, val int) (int, error) {
	var err error
	v := int32(val)

	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(unsafe.Pointer(&v)))
	if e != 0 {
		err = e
	}

	return int(v), err
}

func ioctlI(fd uintptr, req uint) (audioBufInfo, error) {
	var err error
	var info audioBufInfo

	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(unsafe.Pointer(&info)))
	if e != 0 {
		err = e
	}

	return info, err
}
