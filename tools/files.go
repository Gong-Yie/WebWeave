package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"webweave/workspace"
)

func (e *Executor) createFile(ctx context.Context, id string, args Args) (any, error) {
	if args.Content == nil || strings.TrimSpace(args.FilePath) == "" || strings.HasSuffix(args.FilePath, "/") || strings.HasSuffix(args.FilePath, `\`) {
		return nil, errors.New("file_path 必须指向文件，content 必须提供")
	}
	for _, part := range strings.FieldsFunc(args.FilePath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return nil, errors.New("file_path 不允许目录穿越")
		}
	}
	path, err := workspace.ResolveWithin(e.runRoot("result", id), args.FilePath, false)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := workspace.WriteAtomic(path, []byte(*args.Content), 0644); err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "size": len([]byte(*args.Content))}, nil
}

func (e *Executor) readFile(id string, args Args) (any, error) {
	maximum := 20000
	if args.MaxChars != nil {
		maximum = *args.MaxChars
	}
	if maximum < 1 || maximum > 100000 || strings.TrimSpace(args.FilePath) == "" {
		return nil, errors.New("file_path 不能为空，max_chars 必须在 1 到 100000 之间")
	}
	raw := filepath.FromSlash(args.FilePath)
	path := raw
	if !filepath.IsAbs(path) {
		parts := strings.Split(filepath.ToSlash(raw), "/")
		if len(parts) > 1 && (parts[0] == "resources" || parts[0] == "result" || parts[0] == "download") {
			path = filepath.Join(e.runRoot(parts[0], id), filepath.FromSlash(strings.Join(parts[1:], "/")))
		} else if parts[0] == "data" {
			path = filepath.Join(e.root, raw)
		} else {
			path = filepath.Join(e.runRoot("result", id), raw)
		}
	}
	path, err := workspace.CanonicalPath(path)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, scope := range []string{"resources", "result", "download"} {
		root, err := workspace.CanonicalPath(e.runRoot(scope, id))
		if err != nil {
			return nil, err
		}
		allowed = allowed || workspace.Within(root, path)
	}
	if !allowed {
		return nil, errors.New("file_path 越权")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(maximum+1)*4))
	if err != nil {
		return nil, err
	}
	runes := []rune(strings.ToValidUTF8(string(data), "�"))
	truncated := len(runes) > maximum
	if truncated {
		runes = runes[:maximum]
	}
	return map[string]any{"path": path, "content": string(runes), "truncated": truncated, "max_chars": maximum}, nil
}
