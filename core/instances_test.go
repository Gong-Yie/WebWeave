package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"webweave/tools"
	"webweave/workspace"
)

func TestMultipleInstancesLegacyMigration(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "legacy-migration")
	legacy := &instanceRecord{InstanceID: "legacy", PreviewURL: previewURL(run.store.id)}
	if err := workspace.WriteJSONAtomic(filepath.Join(run.store.dir, "state.json"), runState{RunID: run.store.id, Instance: legacy}); err != nil {
		t.Fatal(err)
	}
	if err := run.store.saveInstance(&instanceRecord{InstanceID: "new-instance"}); err != nil {
		t.Fatal(err)
	}
	state, err := run.store.readState()
	if err != nil || len(state.Instances) != 2 {
		t.Fatalf("legacy instance was not preserved: %+v; %v", state, err)
	}
	for _, instance := range state.Instances {
		if instance.PreviewURL != instancePreviewURL(run.store.id, instance.InstanceID) {
			t.Fatal("migrated preview URL still points at the latest instance")
		}
	}
}

func TestMultipleInstances(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "multiple-instances")
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "& '" + strings.ReplaceAll(program, "'", "''") + "' '-test.run=TestInstanceHelper' -- {host} {port}"
	ctx, cancel := context.WithTimeout(context.Background(), 20*second)
	defer cancel()
	first, err := app.instanceAction(ctx, run.store.id, tools.Args{Action: "start", Command: command})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/" + run.store.id + "/instance"
	response := apiRequest(t, app, "POST", path, map[string]any{"action": "start", "timeout": 10})
	assertStatus(t, response, 200)
	var second instanceRecord
	if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if first.InstanceID == second.InstanceID || first.Port == second.Port || first.LogPath == second.LogPath {
		t.Fatal("instances share an ID, port or log file")
	}
	for _, record := range []*instanceRecord{first.instanceRecord, &second} {
		response := apiRequest(t, app, "GET", record.PreviewURL, nil)
		assertStatus(t, response, 200)
		if !strings.Contains(response.Body.String(), "fixture instance "+record.InstanceID) {
			t.Fatalf("preview reached the wrong instance: %s", response.Body.String())
		}
	}
	assertStatus(t, apiRequest(t, app, "POST", path, map[string]any{"action": "stop", "instance_id": "not-in-this-run"}), 404)
	assertStatus(t, apiRequest(t, app, "POST", path, map[string]any{"action": "stop", "instance_id": first.InstanceID}), 200)
	if !healthy(ctx, &second) {
		t.Fatal("stopping the first instance interrupted the second")
	}
	assertStatus(t, apiRequest(t, app, "POST", path, map[string]any{"action": "restart", "instance_id": first.InstanceID, "timeout": 10}), 200)
	state, err := run.store.readState()
	if err != nil || len(state.Instances) != 2 || state.Instance.InstanceID != second.InstanceID || findInstance(state, first.InstanceID).Revision != first.Revision+1 {
		t.Fatalf("instance persistence/restart mismatch: %+v; %v", state, err)
	}
	if *findInstance(state, second.InstanceID).PID != *second.PID || !healthy(ctx, &second) {
		t.Fatal("restarting the first instance changed the second")
	}
	restored, err := New(app.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restored.Close)
	detail, err := restored.detail(run.store.id)
	if err != nil || len(detail.Instances) != 2 || detail.Instances[0].Status != "running" || detail.Instances[1].Status != "running" {
		t.Fatalf("restoring multi-instance history failed: %+v; %v", detail, err)
	}
	if _, err := app.instanceAction(ctx, run.store.id, tools.Args{Action: "start", Command: "throw 'synthetic failure'; # {host} {port}"}); err == nil {
		t.Fatal("failing launch was reported successful")
	}
	if !healthy(ctx, &second) {
		t.Fatal("a failed launch interrupted another instance")
	}
	firstPID := *findInstance(state, first.InstanceID).PID
	if err := app.deleteRun(run.store.id); err != nil {
		t.Fatal(err)
	}
	if tools.ProcessIdentity(firstPID) != "" || tools.ProcessIdentity(*second.PID) != "" {
		t.Fatal("deleting a run left an instance process alive")
	}
}

func TestMultipleInstancePreviewRoutes(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "multiple-previews")
	for _, id := range []string{"preview-a", "preview-b"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/":
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, "<html><head></head><body>%s</body></html>", id)
			case "/asset.js":
				fmt.Fprint(w, id)
			case "/redirect":
				http.Redirect(w, r, "/asset.js", http.StatusFound)
			case "/ws":
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(id))
			}
		}))
		t.Cleanup(server.Close)
		if err := run.store.saveInstance(&instanceRecord{InstanceID: id, Status: "running", AppURL: server.URL}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	for _, id := range []string{"preview-a", "preview-b"} {
		t.Run(id, func(t *testing.T) {
			base := instancePreviewURL(run.store.id, id)
			response := apiRequest(t, app, "GET", base, nil)
			assertStatus(t, response, 200)
			if !strings.Contains(response.Body.String(), `<base href="`+base+`">`) || !strings.Contains(response.Body.String(), "<body>"+id) {
				t.Fatal("instance-specific HTML/base path missing")
			}
			for _, source := range []string{"referer", "cookie"} {
				request := httptest.NewRequest("GET", "/asset.js", nil)
				if source == "referer" {
					request.Header.Set("Referer", server.URL+base)
				} else {
					request.AddCookie(&http.Cookie{Name: "ww_preview_run", Value: run.store.id + ":" + id})
				}
				response := httptest.NewRecorder()
				app.ServeHTTP(response, request)
				if response.Body.String() != id {
					t.Fatalf("%s asset request reached another instance: %s", source, response.Body.String())
				}
			}
			response = apiRequest(t, app, "GET", base+"redirect", nil)
			if response.Header().Get("Location") != base+"asset.js" {
				t.Fatal("redirect escaped its instance")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+base+"ws", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			_, data, err := conn.Read(ctx)
			if err != nil || string(data) != id {
				t.Fatalf("WebSocket reached another instance: %s; %v", data, err)
			}
		})
	}
	assertStatus(t, apiRequest(t, app, "GET", instancePreviewURL(run.store.id, "unknown"), nil), 404)
}
