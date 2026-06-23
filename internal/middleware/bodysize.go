package middleware

import (
	"errors"
	"io"
	"net/http"
)

const maxBodyBytes = 1 << 20 // 1 MB

// MaxBodySize limits the request body to 1 MB and returns 413 on overflow.
func MaxBodySize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// IsBodyTooLarge reports whether err came from MaxBytesReader.
func IsBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr) || errors.Is(err, io.ErrUnexpectedEOF)
}
