package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestSearchAndDownload(t *testing.T) {
	results, err := parseSearchResults([]byte(`<a class="result__a" href="https://example.com">文档 <b>标题</b></a><a class="result__snippet">示例摘要</a>`))
	if err != nil || len(results) != 1 || results[0].Title != "文档 标题" || results[0].Snippet != "示例摘要" {
		t.Fatalf("search parsing: %+v %v", results, err)
	}
	app := testExecutor(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "下载内容") }))
	t.Cleanup(server.Close)
	value, err := app.webSearch(context.Background(), "download-run", Args{DownloadURL: server.URL + "/file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(value.(map[string]any)["path"].(string))
	if err != nil || string(data) != "下载内容" {
		t.Fatal("download mismatch")
	}
	for _, args := range []Args{{DownloadURL: "file:///secret"}, {DownloadURL: server.URL, FileName: "../outside"}, {}} {
		if _, err := app.webSearch(context.Background(), "download-run", args); err == nil {
			t.Fatal("invalid download accepted")
		}
	}
}
