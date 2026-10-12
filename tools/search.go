package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/net/html"

	"webweave/workspace"
)

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func nodeText(node *html.Node) string {
	var text strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(node)
	return strings.Join(strings.Fields(text.String()), " ")
}

func parseSearchResults(data []byte) ([]searchResult, error) {
	document, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	results := []searchResult{}
	var current *searchResult
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		class, href := "", ""
		for _, attr := range node.Attr {
			if attr.Key == "class" {
				class = " " + attr.Val + " "
			}
			if attr.Key == "href" {
				href = attr.Val
			}
		}
		if node.Data == "a" && strings.Contains(class, " result__a ") {
			current = &searchResult{Title: nodeText(node), URL: href}
		}
		if current != nil && strings.Contains(class, " result__snippet ") {
			current.Snippet = nodeText(node)
			if current.Title != "" && current.URL != "" {
				results = append(results, *current)
			}
			current = nil
		}
		for child := node.FirstChild; child != nil && len(results) < 5; child = child.NextSibling {
			visit(child)
		}
	}
	visit(document)
	return results, nil
}

func (e *Executor) webSearch(ctx context.Context, id string, args Args) (any, error) {
	address := args.DownloadURL
	if address == "" {
		if strings.TrimSpace(args.Query) == "" {
			return nil, errors.New("搜索关键词不能为空，或提供 download_url")
		}
		address = "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(args.Query)
	}
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("URL 必须是有效的 http 或 https 地址")
	}
	request, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "WebWeave/1.0")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("网页请求失败: HTTP %d", response.StatusCode)
	}
	if args.DownloadURL != "" {
		name := strings.TrimSpace(args.FileName)
		if name == "" {
			name = filepath.Base(parsed.Path)
			if name == "." || name == "/" || name == "" {
				name = "download"
			}
		}
		if !workspace.ValidFileName(name) {
			return nil, errors.New("file_name 必须是单个文件名")
		}
		target, err := workspace.ResolveWithin(e.runRoot("download", id), name, false)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return nil, err
		}
		file, err := os.CreateTemp(filepath.Dir(target), ".download-*.tmp")
		if err != nil {
			return nil, err
		}
		defer os.Remove(file.Name())
		size, copyErr := io.Copy(file, response.Body)
		closeErr := file.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err := os.Rename(file.Name(), target); err != nil {
			return nil, err
		}
		return map[string]any{"run_id": id, "url": address, "path": target, "size": size}, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	results, err := parseSearchResults(data)
	if err != nil {
		return nil, err
	}
	raw := ""
	if len(results) == 0 {
		raw = string(data[:min(len(data), 4000)])
	}
	return map[string]any{"query": args.Query, "engine": "DuckDuckGo HTML", "results": results, "raw": raw}, nil
}
