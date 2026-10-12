package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// isAbsolutePath reports whether a command refers to a fixed location.
//
// The check is deliberately OS-aware: on Windows an absolute path has a drive letter or
// a UNC prefix, neither of which a leading "/" would satisfy, so a POSIX-style test
// would send a perfectly good Windows path down the PATH-lookup branch.
func isAbsolutePath(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) {
		return true
	}
	if runtime.GOOS == "windows" {
		// UNC paths such as \\server\share are absolute on Windows without a drive.
		if len(p) > 1 && p[0] == '\\' && p[1] == '\\' {
			return true
		}
	}
	return false
}

// fileExists reports whether a path exists and is not a directory.
func fileExists(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// dirExists reports whether a path exists and is a directory.
func dirExists(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// isExecutableFile reports whether a path is a regular file that can be executed.
//
// On Unix the execute bit is the criterion. On Windows there is no execute bit, so
// existence as a file is the best available answer, and the real test is whether the
// spawn succeeds.
func isExecutableFile(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}
