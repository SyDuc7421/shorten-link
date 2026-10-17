package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/april/shorten-link/internal/ctxkey"
	"github.com/april/shorten-link/internal/logger"
	"github.com/april/shorten-link/internal/middleware"
)

func TestAccessLog_LogsOneLinePerRequest(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	})
	h := middleware.AccessLog(next)

	req := httptest.NewRequest("POST", "/api/links?secret=x", nil)
	ctx := logger.WithContext(req.Context(), zap.New(core))
	req = req.WithContext(ctxkey.SetUserID(ctx, 42))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if logs.Len() != 1 {
		t.Fatalf("want 1 log line, got %d", logs.Len())
	}
	f := logs.All()[0].ContextMap()
	if f["method"] != "POST" || f["path"] != "/api/links" {
		t.Errorf("method/path wrong: %v", f)
	}
	if f["status"] != int64(http.StatusCreated) {
		t.Errorf("want status 201, got %v", f["status"])
	}
	if f["bytes"] != int64(5) {
		t.Errorf("want bytes 5, got %v", f["bytes"])
	}
	if f["user_id"] != int64(42) {
		t.Errorf("want user_id 42, got %v", f["user_id"])
	}
	if _, ok := f["duration"]; !ok {
		t.Error("missing duration")
	}
}

func TestAccessLog_DefaultsStatusTo200(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(logger.WithContext(req.Context(), zap.New(core)))
	middleware.AccessLog(next).ServeHTTP(httptest.NewRecorder(), req)

	if got := logs.All()[0].ContextMap()["status"]; got != int64(http.StatusOK) {
		t.Errorf("want 200, got %v", got)
	}
}

func TestAccessLog_LogsStatusSetByRecover(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { panic("boom") })

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(logger.WithContext(req.Context(), zap.New(core)))
	middleware.AccessLog(middleware.Recover(next)).ServeHTTP(httptest.NewRecorder(), req)

	access := logs.FilterMessage("request").All()
	if len(access) != 1 || access[0].ContextMap()["status"] != int64(http.StatusInternalServerError) {
		t.Errorf("want one access line with status 500, got %v", access)
	}
}
