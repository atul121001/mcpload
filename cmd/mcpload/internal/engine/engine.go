// Package engine carries the load engine (a k6 build with the xk6-mcpload
// extension) and the bundled scenarios inside release builds of mcpload, so
// users install and run a single program.
//
// Release builds use the build tag `embedengine` and expect, at build time:
//
//	internal/engine/bin/engine      the k6 binary for the target os/arch
//	internal/engine/bin/scenarios/  a copy of the repo's scenarios/ folder
//
// (see .github/workflows/release.yml; the bin/ folder is gitignored). Builds
// without the tag embed nothing, and mcpload falls back to a k6 found next to
// it, in the current folder or on PATH.
//
// On first use the payloads are extracted to the user cache folder
// (os.UserCacheDir()/mcpload, or $MCPLOAD_CACHE_DIR) under a folder named
// after their SHA-256, written atomically (temp file or folder + rename) and
// reused by later runs after checking the hash.
package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
)

// ErrNotEmbedded is returned by Path and ScenariosDir in builds without the
// embedded payload.
var ErrNotEmbedded = errors.New("this mcpload build has no embedded engine")

// Embedded reports whether this build carries the engine.
func Embedded() bool { return len(enginePayload) > 0 }

// HasScenarios reports whether this build carries the bundled scenarios.
func HasScenarios() bool { return embeddedScenarios() != nil }

var (
	pathOnce sync.Once
	pathVal  string
	pathErr  error

	scnOnce sync.Once
	scnVal  string
	scnErr  error
)

// Path returns the path of the embedded engine, extracting it on first use.
func Path() (string, error) {
	pathOnce.Do(func() {
		if !Embedded() {
			pathErr = ErrNotEmbedded
			return
		}
		root, err := cacheRoot()
		if err != nil {
			pathErr = err
			return
		}
		pathVal, pathErr = extractFile(enginePayload, filepath.Join(root, "engine"), binaryName())
	})
	return pathVal, pathErr
}

// ScenariosDir returns a folder that contains scenarios/ with the bundled
// scenarios, extracting them on first use.
func ScenariosDir() (string, error) {
	scnOnce.Do(func() {
		fsys := embeddedScenarios()
		if fsys == nil {
			scnErr = ErrNotEmbedded
			return
		}
		root, err := cacheRoot()
		if err != nil {
			scnErr = err
			return
		}
		scnVal, scnErr = extractTree(fsys, filepath.Join(root, "scenarios"), "scenarios")
	})
	return scnVal, scnErr
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "mcpload-engine.exe"
	}
	return "mcpload-engine"
}

// cacheRoot is $MCPLOAD_CACHE_DIR, else os.UserCacheDir()/mcpload.
func cacheRoot() (string, error) {
	if d := os.Getenv("MCPLOAD_CACHE_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("engine: no cache folder (set MCPLOAD_CACHE_DIR): %w", err)
	}
	return filepath.Join(d, "mcpload"), nil
}

// extractFile writes payload to <root>/<sha256 prefix>/<name> unless a file
// with the same hash is already there, and returns its path.
func extractFile(payload []byte, root, name string) (string, error) {
	sum := sha256.Sum256(payload)
	dir := filepath.Join(root, hex.EncodeToString(sum[:8]))
	dst := filepath.Join(dir, name)
	if fileHasHash(dst, sum, int64(len(payload))) {
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("engine: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("engine: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return "", fmt.Errorf("engine: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("engine: write %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", fmt.Errorf("engine: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// Another mcpload may have extracted it at the same time (on Windows a
		// running executable cannot be replaced).
		if fileHasHash(dst, sum, int64(len(payload))) {
			return dst, nil
		}
		return "", fmt.Errorf("engine: %w", err)
	}
	if !fileHasHash(dst, sum, int64(len(payload))) {
		return "", fmt.Errorf("engine: %s does not match the embedded engine after extraction", dst)
	}
	return dst, nil
}

func fileHasHash(p string, sum [32]byte, size int64) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() != size {
		return false
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), sum[:])
}

// completeMarker is written last into an extracted tree; a tree without it is
// ignored and rebuilt.
const completeMarker = ".complete"

// extractTree copies fsys into <root>/<sha256 prefix>/<sub>/ and returns
// <root>/<sha256 prefix>. The tree is built in a temp folder and renamed into
// place, so readers never see a partial copy.
func extractTree(fsys fs.FS, root, sub string) (string, error) {
	files, sum, err := hashTree(fsys)
	if err != nil {
		return "", fmt.Errorf("engine: scenarios: %w", err)
	}
	dst := filepath.Join(root, hex.EncodeToString(sum[:8]))
	if isFile(filepath.Join(dst, completeMarker)) {
		return dst, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("engine: %w", err)
	}
	tmp, err := os.MkdirTemp(root, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("engine: %w", err)
	}
	defer os.RemoveAll(tmp) // no-op once renamed
	for _, name := range files {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return "", fmt.Errorf("engine: scenarios: %w", err)
		}
		p := filepath.Join(tmp, sub, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", fmt.Errorf("engine: %w", err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return "", fmt.Errorf("engine: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, completeMarker), nil, 0o644); err != nil {
		return "", fmt.Errorf("engine: %w", err)
	}
	if isFile(filepath.Join(dst, completeMarker)) {
		return dst, nil // extracted by another mcpload meanwhile
	}
	_ = os.RemoveAll(dst) // a partial tree from an interrupted older version
	if err := os.Rename(tmp, dst); err != nil {
		if isFile(filepath.Join(dst, completeMarker)) {
			return dst, nil
		}
		return "", fmt.Errorf("engine: %w", err)
	}
	return dst, nil
}

// hashTree lists the regular files of fsys (sorted) and hashes their names
// and contents.
func hashTree(fsys fs.FS) ([]string, [32]byte, error) {
	var files []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, [32]byte{}, err
	}
	if len(files) == 0 {
		return nil, [32]byte{}, errors.New("no files")
	}
	sort.Strings(files)
	h := sha256.New()
	for _, name := range files {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, [32]byte{}, err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", path.Clean(name), len(data))
		h.Write(data)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return files, sum, nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
