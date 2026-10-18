package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	if maximum < 1 || maximum > 100000 || args.Offset < 0 || strings.TrimSpace(args.FilePath) == "" {
		return nil, errors.New("file_path 不能为空，offset 必须非负，max_chars 必须在 1 到 100000 之间")
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
	hash := sha256.New()
	reader := bufio.NewReader(io.TeeReader(file, hash))
	for i := 0; i < args.Offset; i++ {
		if _, _, err := reader.ReadRune(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("offset 超过文件字符数")
			}
			return nil, err
		}
	}
	runes := make([]rune, 0, maximum+1)
	for len(runes) <= maximum {
		value, _, err := reader.ReadRune()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		runes = append(runes, value)
	}
	truncated := len(runes) > maximum
	if truncated {
		runes = runes[:maximum]
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "content": string(runes), "truncated": truncated, "max_chars": maximum, "offset": args.Offset, "next_offset": args.Offset + len(runes), "content_sha256": hex.EncodeToString(hash.Sum(nil))}, nil
}
