//go:build linux && (mips || mipsle || mips64 || mips64le)

package server

// soReusePort is SO_REUSEPORT on Linux/MIPS, whose socket ABI differs from
// asm-generic.
const soReusePort = 0x200
