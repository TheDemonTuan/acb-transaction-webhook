//go:build !windows

package main

import (
	"os"
	"syscall"
)

func importFilePermissionsOK(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().Perm() == 0600 && stat.Uid == 1000 && stat.Gid == 1000
}
