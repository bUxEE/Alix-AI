// Package rootguard decides which directories may never be the root of a crawl: the readiness scanner's project
// directory and a RAG folder source. It refuses a drive or filesystem root, the operating system's own directories
// and the home directory itself. It imports nothing of AlixAI, so both can use it.
package rootguard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Refusal explains why a directory may not be crawled, or returns "" when it may. abs is an absolute path.
func Refusal(abs string) string { return RefusalWith(abs, os.Getenv, runtime.GOOS) }

// RefusalWith is Refusal on the environment getenv reads and the operating system goos names; tests use it to
// check the rules of every system on one.
func RefusalWith(abs string, getenv func(string) string, goos string) string {
	clean := filepath.Clean(abs)
	if filepath.Dir(clean) == clean {
		return "the directory is a filesystem or drive root"
	}
	norm := func(p string) string {
		p = filepath.ToSlash(filepath.Clean(p))
		if goos == "windows" {
			p = strings.ToLower(p)
		}
		return strings.TrimRight(p, "/")
	}
	target := norm(clean)
	if vol := filepath.VolumeName(clean); vol != "" && target == norm(vol) {
		return "the directory is a drive root"
	}
	exact := map[string]bool{}
	var under []string
	if goos == "windows" {
		for _, k := range []string{"SystemRoot", "windir", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
			if v := getenv(k); v != "" {
				under = append(under, norm(v))
			}
		}
		for _, k := range []string{"ProgramData", "USERPROFILE", "HOME"} {
			if v := getenv(k); v != "" {
				exact[norm(v)] = true
			}
		}
		if sd := getenv("SystemDrive"); sd != "" {
			exact[norm(sd+"/Users")] = true
		}
	} else {
		under = []string{"/proc", "/sys", "/dev", "/etc", "/boot", "/bin", "/sbin", "/lib", "/lib64", "/usr/bin", "/usr/sbin", "/usr/lib"}
		for _, p := range []string{"/usr", "/var", "/opt", "/home", "/tmp", "/root", "/srv", "/mnt", "/media", "/Users", "/System", "/Library"} {
			exact[p] = true
		}
		if v := getenv("HOME"); v != "" {
			exact[norm(v)] = true
		}
	}
	if exact[target] {
		return "the directory is a system or home directory"
	}
	for _, u := range under {
		if u != "" && (target == u || strings.HasPrefix(target, u+"/")) {
			return "the directory is inside a system directory"
		}
	}
	return ""
}
