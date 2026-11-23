package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
)

// writeFileAtomic writes data to a temporary file in the destination
// directory and renames it over path, so readers never see a partial file
// and a failed run never truncates an existing one. An existing non-regular
// destination (such as /dev/stdout or a named pipe) is written in place.
func writeFileAtomic(path string, data []byte) (err error) {
	if fi, statErr := os.Stat(path); statErr == nil && !fi.Mode().IsRegular() {
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory", path)
		}
		f, openErr := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // G304: path is the user's --out
		if openErr != nil {
			return openErr
		}
		if _, err = f.Write(data); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// The export is a schema description, not a secret: use the usual
	// permissions of a generated file instead of CreateTemp's 0600.
	if err = os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // G302: non-secret output file
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ResolveVersion returns v when set (from -ldflags "-X main.version=..."),
// otherwise the module version or VCS revision recorded by the Go toolchain,
// otherwise "dev".
func ResolveVersion(v string) string {
	if v != "" {
		return v
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if mv := info.Main.Version; mv != "" && mv != "(devel)" {
		return mv
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified == "true" {
		rev += "-dirty"
	}
	return fmt.Sprintf("dev-%s", rev)
}
