package core

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"webweave/workspace"
)

type templateRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RunID     string `json:"run_id"`
	CreatedAt string `json:"created_at"`
	FileCount int    `json:"file_count"`
	Path      string `json:"path"`
}

type templateSaveRequest struct {
	Name         string `json:"name"`
	Approved     bool   `json:"approved"`
	SourceFolder string `json:"source_folder,omitempty"`
}

func (a *App) listTemplates() ([]templateRecord, error) {
	a.templateMu.Lock()
	defer a.templateMu.Unlock()
	root := filepath.Join(a.root, "data", "templates")
	entries, err := os.ReadDir(root)
	result := []templateRecord{}
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !workspace.ValidName(entry.Name()) {
			continue
		}
		path, err := workspace.ResolveWithin(root, filepath.Join(entry.Name(), "template.json"), false)
		if err != nil {
			return nil, err
		}
		var record templateRecord
		if err := readJSONFile(path, &record); err != nil {
			return nil, err
		}
		if record.ID == entry.Name() {
			result = append(result, record)
		}
	}
	metadataRoot := filepath.Join(root, ".metadata")
	metadata, err := os.ReadDir(metadataRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range metadata {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !workspace.ValidName(strings.TrimSuffix(entry.Name(), ".json")) {
			continue
		}
		path, err := workspace.ResolveWithin(metadataRoot, entry.Name(), false)
		if err != nil {
			return nil, err
		}
		var record templateRecord
		if err := readJSONFile(path, &record); err != nil {
			return nil, err
		}
		if record.ID == strings.TrimSuffix(entry.Name(), ".json") {
			result = append(result, record)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].CreatedAt < result[j].CreatedAt })
	return result, nil
}

func (a *App) saveTemplate(id string, request templateSaveRequest) (templateRecord, error) {
	var record templateRecord
	if !request.Approved {
		return record, apiError{400, "APPROVAL_REQUIRED", "请检查生成结果并确认保存到模板库"}
	}
	name := strings.TrimSpace(request.Name)
	if !validTemplateName(name) {
		return record, apiError{400, "INVALID_TEMPLATE_NAME", "模板名称须为 1 至 120 个字符，且必须是合法文件夹名称"}
	}
	sourceName := request.SourceFolder
	if sourceName == "" {
		sourceName = name
	}
	if !validTemplateName(sourceName) {
		return record, apiError{400, "INVALID_TEMPLATE_NAME", "模板源文件夹名称无效"}
	}
	run, err := a.requireRun(id)
	if err != nil {
		return record, err
	}
	// Prevent continuation or deletion while taking the approved snapshot.
	run.mu.Lock()
	defer run.mu.Unlock()
	state, err := run.store.readState()
	if err != nil {
		return record, err
	}
	if run.job != nil || state.Status != "completed" || state.AgentID != "template" {
		return record, apiError{409, "TEMPLATE_NOT_READY", "请等待前端模板制作 Agent 完成任务后再保存"}
	}
	a.templateMu.Lock()
	defer a.templateMu.Unlock()
	library := filepath.Join(a.root, "data", "templates")
	target, err := workspace.ResolveWithin(library, name, false)
	if err != nil {
		return record, apiError{400, "INVALID_TEMPLATE_NAME", err.Error()}
	}
	if _, err := os.Lstat(target); err == nil {
		return record, apiError{409, "TEMPLATE_EXISTS", "同名模板已存在，请修改模板名称"}
	} else if !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	source, err := workspace.ResolveWithin(a.runRoot("result", id), sourceName, false)
	if err != nil {
		return record, apiError{400, "INVALID_TEMPLATE_FILES", err.Error()}
	}
	if err := os.MkdirAll(library, 0755); err != nil {
		return record, err
	}
	temporary, err := os.MkdirTemp(library, ".pending-")
	if err != nil {
		return record, err
	}
	defer func() {
		if err := os.RemoveAll(temporary); err != nil {
			slog.Warn("清理模板暂存目录失败", "error", err)
		}
	}()
	count, err := copyTemplateFiles(source, temporary)
	if err != nil {
		return record, apiError{400, "INVALID_TEMPLATE_FILES", err.Error()}
	}
	if count == 0 {
		return record, apiError{400, "EMPTY_TEMPLATE", "当前结果目录没有模板文件"}
	}
	record = templateRecord{workspace.NewID(), name, id, workspace.Timestamp(), count, ""}
	record.Path = filepath.ToSlash(filepath.Join("data", "templates", name))
	if err := os.Rename(temporary, target); err != nil {
		return templateRecord{}, err
	}
	metadataPath, err := workspace.ResolveWithin(library, filepath.Join(".metadata", record.ID+".json"), false)
	if err == nil {
		err = workspace.WriteJSONAtomic(metadataPath, record)
	}
	if err != nil {
		if cleanupErr := os.RemoveAll(target); cleanupErr != nil {
			slog.Error("回滚模板保存失败", "error", cleanupErr)
		}
		return templateRecord{}, err
	}
	return record, nil
}

func validTemplateName(name string) bool {
	if name == "" || len([]rune(name)) > 120 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || strings.ContainsAny(name, "/\\<>:\"|?*\x00\r\n") {
		return false
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" {
		return false
	}
	return !(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9')
}

// The library contract is a named folder containing exactly these three files.
func copyTemplateFiles(source, destination string) (int, error) {
	entries, err := os.ReadDir(source)
	if err != nil {
		return 0, err
	}
	if len(entries) != 3 {
		return 0, errors.New("模板文件夹内只能有 index.html、style.css 和 script.js 三个文件")
	}
	var total int64
	for _, name := range []string{"index.html", "style.css", "script.js"} {
		path := filepath.Join(source, name)
		info, err := os.Lstat(path)
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("模板不能包含目录、符号链接或其他非普通文件: %s", name)
		}
		total += info.Size()
		if total > 100*1024*1024 {
			return 0, errors.New("模板超过 100 MB")
		}
		if name == "index.html" {
			if err := validateTemplateHTML(path); err != nil {
				return 0, err
			}
		}
		if err := copyTemplateFile(path, filepath.Join(destination, name)); err != nil {
			return 0, err
		}
	}
	return 3, nil
}

var templateLabel = regexp.MustCompile(`^(标题|文本|组件)[0-9]+$`)
var templateHeading = regexp.MustCompile(`^标题[0-9]+$`)

func validateTemplateHTML(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	document, err := html.Parse(file)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	css, js := false, false
	var visit func(*html.Node, bool) error
	visit = func(node *html.Node, inHead bool) error {
		if node.Type == html.ElementNode && (node.Data == "script" || node.Data == "style") {
			for _, attr := range node.Attr {
				if node.Data == "script" && attr.Key == "src" && (attr.Val == "script.js" || attr.Val == "./script.js") {
					js = true
				}
			}
			return nil
		}
		inHead = inHead || node.Data == "head"
		for _, attr := range node.Attr {
			if node.Data == "link" && attr.Key == "href" && (attr.Val == "style.css" || attr.Val == "./style.css") {
				css = true
			}
			if !inHead && attr.Val != "" && (attr.Key == "id" || attr.Key == "aria-label" || attr.Key == "title" || attr.Key == "placeholder" || attr.Key == "alt") {
				if !templateLabel.MatchString(attr.Val) {
					return fmt.Errorf("模板组件命名必须为标题、文本或组件加数字 id: %s", attr.Val)
				}
				if attr.Key == "id" {
					if ids[attr.Val] {
						return fmt.Errorf("模板 id 重复: %s", attr.Val)
					}
					ids[attr.Val] = true
				}
			}
		}
		if node.Type == html.TextNode && (!inHead || (node.Parent != nil && node.Parent.Data == "title")) {
			text := strings.TrimSpace(node.Data)
			if text != "" {
				if !templateLabel.MatchString(text) {
					return fmt.Errorf("模板文字必须为标题、文本或组件加数字 id: %s", shortenText(text, 60))
				}
				for parent := node.Parent; parent != nil; parent = parent.Parent {
					if (parent.Data == "title" || (len(parent.Data) == 2 && parent.Data[0] == 'h' && parent.Data[1] >= '1' && parent.Data[1] <= '6')) && !templateHeading.MatchString(text) {
						return errors.New("标题必须使用标题加数字 id")
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if err := visit(child, inHead); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(document, false); err != nil {
		return err
	}
	if !css || !js {
		return errors.New("index.html 必须引用同目录的 style.css 和 script.js")
	}
	return nil
}

func copyTemplateFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	closeErr := output.Close()
	if err != nil {
		return err
	}
	return closeErr
}
