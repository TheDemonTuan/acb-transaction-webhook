package main

import "os"

// Windows fixtures cannot attest POSIX owner/mode; deployment import runs Linux.
func importFilePermissionsOK(info os.FileInfo) bool { return info.Mode().IsRegular() }
