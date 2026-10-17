package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/april/shorten-link/internal/logger"
	"github.com/april/shorten-link/internal/middleware"
)

func TestRecover_PanicReturnsJSON500(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { panic("boom") })
	h := middleware.Recover(next)

	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(logger.WithContext(req.Context(), zap.New(core)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("want application/json, got %q", ct)
	}
	if body := w.Body.String(); body != `{"error":"internal server error"}`+"\n" {
		t.Errorf("unexpected body %q", body)
	}
	if logs.Len() != 1 {
		t.Fatalf("want 1 error log, got %d", logs.Len())
	}
}

func TestRecover_PanicAfterWriteKeepsStatus(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("late")
	})
	w := httptest.NewRecorder()
	middleware.Recover(next).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if w.Code != http.StatusAccepted {
		t.Errorf("want original 202, got %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("want no 500 body appended, got %q", w.Body.String())
	}
}

func TestRecover_ReraisesErrAbortHandler(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { panic(http.ErrAbortHandler) })
	defer func() {
		p := recover()
		if err, ok := p.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("want ErrAbortHandler re-panicked, got %v", p)
		}
	}()
	middleware.Recover(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}
