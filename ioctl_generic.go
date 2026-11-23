//go:build linux && (386 || amd64 || arm || arm64 || loong64 || riscv64 || s390x)

package oss

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2

	iocSizebits = 14
)
