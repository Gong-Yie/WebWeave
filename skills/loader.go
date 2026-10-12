package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"webweave/workspace"
)

type Document struct {
	Name         string `json:"name" yaml:"name"`
	Description  string `json:"description" yaml:"description"`
	Path         string `json:"path" yaml:"-"`
	Instructions string `json:"instructions,omitempty" yaml:"-"`
}

type Listing struct {
	Skills []Document `json:"skills"`
	Errors []string   `json:"errors"`
}

func (l *Loader) readFile(raw string) (Document, error) {
	var document Document
	root := l.root
	path, err := workspace.ResolveWithin(root, raw, false)
	if err != nil {
		return document, err
	}
	if filepath.Base(path) != "SKILL.md" {
		return document, errors.New("Skill 文件名必须是 SKILL.md")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return document, err
	}
	if utf8.RuneCount(data) > 100000 {
		return document, errors.New("SKILL.md 超过 100000 字符")
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return document, errors.New("SKILL.md 必须包含 YAML front matter")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return document, errors.New("YAML front matter 未闭合")
	}
	metadataEnd := end + 4
	bodyStart := metadataEnd + 4
	if bodyStart < len(text) && text[bodyStart] != '\n' {
		return document, errors.New("YAML front matter 分隔符无效")
	}
	if err := yaml.Unmarshal([]byte(text[4:metadataEnd]), &document); err != nil {
		return document, fmt.Errorf("front matter YAML 无效: %w", err)
	}
	if !workspace.ValidName(document.Name) || strings.TrimSpace(document.Description) == "" {
		return document, errors.New("front matter 必须包含合法名称和非空 description")
	}
	document.Description = strings.TrimSpace(document.Description)
	document.Path = filepath.ToSlash(raw)
	document.Instructions = strings.TrimSpace(text[bodyStart:])
	return document, nil
}

func (l *Loader) List() (Listing, error) {
	listing := Listing{Skills: []Document{}, Errors: []string{}}
	entries, err := os.ReadDir(l.root)
	if errors.Is(err, os.ErrNotExist) {
		return listing, nil
	}
	if err != nil {
		return listing, err
	}
	names := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() || !workspace.ValidName(entry.Name()) {
			continue
		}
		document, err := l.readFile(filepath.Join(entry.Name(), "SKILL.md"))
		if err != nil {
			listing.Errors = append(listing.Errors, entry.Name()+": "+err.Error())
			continue
		}
		if names[document.Name] {
			listing.Errors = append(listing.Errors, entry.Name()+": Skill 名称重复")
			continue
		}
		names[document.Name] = true
		document.Instructions = ""
		listing.Skills = append(listing.Skills, document)
	}
	return listing, nil
}

func (l *Loader) Read(name string) (Document, error) {
	if !workspace.ValidName(name) {
		return Document{}, errors.New("Skill 名称无效")
	}
	listing, err := l.List()
	if err != nil {
		return Document{}, err
	}
	for _, skill := range listing.Skills {
		if skill.Name == name {
			return l.readFile(skill.Path)
		}
	}
	return Document{}, fmt.Errorf("未找到 Skill: %s", name)
}

type Loader struct{ root string }

func NewLoader(root string) *Loader { return &Loader{root: root} }
