//go:build unix

package outfile

import "syscall"

func setUmask(m int) int { return syscall.Umask(m) }
