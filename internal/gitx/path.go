package gitx

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var (
	msysDrive = regexp.MustCompile(`^/([A-Za-z])/`)
	msysMnt   = regexp.MustCompile(`^/mnt/([A-Za-z])/`)
)

// LooksMSYS reports whether p is an MSYS-style absolute path such as /d/foo or
// /mnt/d/foo. Git for Windows silently resolves these against its own install
// root, so a path that looks harmless can write to the wrong drive.
func LooksMSYS(p string) bool {
	return msysDrive.MatchString(p) || msysMnt.MatchString(p)
}

// NativePath converts an MSYS path to a native Windows path. On other systems
// it returns p unchanged. A path with no drive letter is left alone; callers
// that need a filesystem path should use RequireNative, which rejects it.
func NativePath(p string) string {
	if runtime.GOOS != "windows" || p == "" {
		return p
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if m := msysMnt.FindStringSubmatch(p); m != nil {
		return strings.ToUpper(m[1]) + ":" + p[len(m[0])-1:]
	}
	if m := msysDrive.FindStringSubmatch(p); m != nil {
		return strings.ToUpper(m[1]) + ":" + p[len(m[0])-1:]
	}
	// D:\x -> D:/x is already handled above by the backslash rewrite.
	return p
}

// RequireNative rejects an MSYS path and returns a native form.
func RequireNative(p string) (string, error) {
	native := NativePath(p)
	if runtime.GOOS == "windows" && strings.HasPrefix(native, "/") && !strings.HasPrefix(native, "//") {
		return "", fmt.Errorf("path %q has no drive letter on Windows; use a native path like D:/work/repo, not %q", p, native)
	}
	return native, nil
}

// AssertNoMSYSArgs guards a git invocation. Ref names and URLs never look like
// /d/..., so a match means a path slipped through unnormalised.
func AssertNoMSYSArgs(args []string) error {
	for _, a := range args {
		if LooksMSYS(a) {
			return fmt.Errorf("refusing to run git with MSYS path argument %q; convert it with NativePath first", a)
		}
	}
	return nil
}

// FileURL normalises a path into a file:// URL with a native drive letter. It is
// used only for local remotes in tests and for plain-git deployments.
func FileURL(p string) string {
	native := filepath.ToSlash(NativePath(p))
	if strings.HasPrefix(native, "//") {
		return "file:" + native
	}
	if strings.HasPrefix(native, "/") {
		return "file://" + native
	}
	if runtime.GOOS == "windows" && len(native) >= 2 && native[1] == ':' {
		// git-for-windows resolves file:///C:/x to the path /C:/x, which it does
		// not treat as absolute, and the push fails with "does not appear to be
		// a git repository". file://C:/x is accepted there. Verified against
		// git 2.53.0.windows.3.
		return "file://" + native
	}
	return "file:///" + native
}
