package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

func NewID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}

func Timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func Within(root, target string) bool {
	if runtime.GOOS == "windows" {
		root, target = strings.ToLower(root), strings.ToLower(target)
	}
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func ResolveWithin(root, raw string, allowAbsolute bool) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", errors.New("路径包含无效字符")
	}
	if !allowAbsolute && !filepath.IsLocal(raw) {
		return "", errors.New("路径必须是相对路径")
	}
	base, err := CanonicalPath(root)
	if err != nil {
		return "", err
	}
	target := raw
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, raw)
	}
	target, err = CanonicalPath(target)
	if err != nil {
		return "", err
	}
	if !Within(base, target) {
		return "", errors.New("路径越权")
	}
	return target, nil
}

func WriteJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(path, append(data, '\n'), 0600)
}

func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".webweave-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Chmod(temporary, mode); err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		err = os.Rename(temporary, path)
		if err == nil {
			return nil
		}
		if runtime.GOOS != "windows" || !errors.Is(err, os.ErrPermission) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return fmt.Errorf("写入文件失败: %w", err)
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func ValidName(name string) bool { return namePattern.MatchString(name) }

func ScopeRoot(root, scope, id string) string { return filepath.Join(root, "data", scope, id) }

func PrepareDirectories(root, id string) error {
	if !ValidName(id) {
		return errors.New("run_id 无效")
	}
	for _, scope := range []string{"resources", "result", "download", "run"} {
		path, err := ResolveWithin(filepath.Join(root, "data", scope), id, false)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0755); err != nil {
			return err
		}
	}
	return nil
}

func WorkingDirectory(workspaceRoot, id, raw string, resultOnly bool) (string, error) {
	root := ScopeRoot(workspaceRoot, "result", id)
	if raw == "" {
		raw = "."
	}
	path, err := ResolveWithin(root, raw, true)
	if err != nil && !resultOnly && filepath.IsAbs(raw) {
		for _, scope := range []string{"resources", "download"} {
			path, err = ResolveWithin(ScopeRoot(workspaceRoot, scope, id), raw, true)
			if err == nil {
				break
			}
		}
	}
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("cwd 必须是目录")
	}
	return path, nil
}

func MIMEType(path string) string {
	value := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if value == "" {
		return "application/octet-stream"
	}
	return strings.SplitN(value, ";", 2)[0]
}

func ValidFileName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00:")
}
