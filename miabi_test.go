package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientCalls(t *testing.T) {
	var gotScale string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing bearer token on %s", r.URL)
		}
		switch {
		case r.URL.Path == "/api/v1/workspaces/1/apps/7" && r.Method == "GET":
			io.WriteString(w, `{"success":true,"data":{"id":7,"name":"web","replicas":3,"runtime_kind":"service"}}`)
		case r.URL.Path == "/api/v1/workspaces/1/apps/7/status":
			io.WriteString(w, `{"success":true,"data":{"running":true,"service_replicas":3,"service_running_tasks":2}}`)
		case r.URL.Path == "/api/v1/workspaces/1/analytics/summary":
			if r.URL.Query().Get("app") != "7" || r.URL.Query().Get("range") != "5m" {
				t.Errorf("bad analytics query: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"success":true,"data":{"granularity":"minute","series":[{"t":"2026-01-01T10:04:00Z","requests":120,"errors_5xx":2,"p95_latency_ms":31.5}]}}`)
		case r.URL.Path == "/api/v1/workspaces/1/apps/7/scale" && r.Method == "POST":
			b, _ := io.ReadAll(r.Body)
			gotScale = string(b)
			io.WriteString(w, `{"success":true,"data":{"message":"application scaled"}}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"success":false,"error":{"message":"nope"}}`)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/", "1", "secret")
	ctx := context.Background()

	app, err := c.App(ctx, 7)
	if err != nil || app.RuntimeKind != "service" || app.Replicas != 3 {
		t.Fatalf("App: %+v %v", app, err)
	}
	st, err := c.AppStatus(ctx, 7)
	if err != nil || st.ServiceReplicas != 3 || st.ServiceRunningTasks != 2 {
		t.Fatalf("AppStatus: %+v %v", st, err)
	}
	sum, err := c.Analytics(ctx, 7, "5m")
	if err != nil || len(sum.Series) != 1 || sum.Series[0].Requests != 120 || sum.Series[0].Errors5xx != 2 || sum.Series[0].P95Ms != 31.5 {
		t.Fatalf("Analytics: %+v %v", sum, err)
	}
	if err := c.Scale(ctx, 7, 4); err != nil {
		t.Fatal(err)
	}
	var body map[string]int
	if json.Unmarshal([]byte(gotScale), &body) != nil || body["replicas"] != 4 {
		t.Fatalf("scale body = %q", gotScale)
	}
}

func TestClientReportsAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			io.WriteString(w, "<html>bad gateway</html>")
			return
		}
		w.WriteHeader(403)
		io.WriteString(w, `{"success":false,"data":null,"error":{"message":"IP address not allowed for this API key"}}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "1", "k")
	_, err := c.App(context.Background(), 1)
	ae, ok := err.(*APIError)
	if !ok || ae.Status != 403 || !strings.Contains(ae.Message, "IP address not allowed") {
		t.Fatalf("expected APIError 403 with the server message, got %#v", err)
	}
	if _, err := c.AppStatus(context.Background(), 1); err == nil {
		t.Fatal("non-JSON body must be an error")
	}
}
