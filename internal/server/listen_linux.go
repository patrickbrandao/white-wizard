//go:build linux && !mips && !mipsle && !mips64 && !mips64le

package server

// soReusePort is SO_REUSEPORT on Linux. The frozen syscall package omits it on
// linux/amd64 (and 386/arm), so the value comes from the kernel ABI
// (asm-generic), shared by every Linux architecture Go supports except MIPS.
const soReusePort = 0xf
