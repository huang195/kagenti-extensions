package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

func TestListSessions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			t.Errorf("wrong path: %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(struct {
			Sessions []session.SessionSummary `json:"sessions"`
		}{
			Sessions: []session.SessionSummary{{ID: "abc", EventCount: 3}},
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	got, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "abc" || got[0].EventCount != 3 {
		t.Errorf("got %+v", got)
	}
}

func TestGetSession(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/ctx-abc" {
			t.Errorf("wrong path: %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(pipeline.SessionView{
			ID: "ctx-abc",
			Events: []pipeline.SessionEvent{
				{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest},
			},
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	got, err := c.GetSession(context.Background(), "ctx-abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "ctx-abc" || len(got.Events) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestGetSession_404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer ts.Close()
	c := New(ts.URL)
	_, err := c.GetSession(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestEndpointTrimSlash(t *testing.T) {
	c := New("http://localhost:9094/")
	if c.Endpoint() != "http://localhost:9094" {
		t.Errorf("trailing slash not trimmed: %q", c.Endpoint())
	}
}
