package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/april/shorten-link/internal/service"
)

type fakeAdminSetter struct {
	changed     bool
	err         error
	gotEmail    string
	gotGrant    bool
	callCount   int
	created     bool
	gotPassword string
}

func (f *fakeAdminSetter) CreateAdmin(_ context.Context, email, password string) (bool, error) {
	f.callCount++
	f.gotEmail, f.gotPassword = email, password
	return f.created, f.err
}

func (f *fakeAdminSetter) SetAdmin(_ context.Context, email string, admin bool) (bool, error) {
	f.callCount++
	f.gotEmail, f.gotGrant = email, admin
	return f.changed, f.err
}

func TestRunAdminCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		setter   fakeAdminSetter
		wantCode int
		wantOut  string
		wantCall bool
		wantGrnt bool
	}{
		{name: "grant", args: []string{"grant", "a@x.com"}, setter: fakeAdminSetter{changed: true}, wantCode: 0, wantOut: "granted admin to a@x.com", wantCall: true, wantGrnt: true},
		{name: "grant again", args: []string{"grant", "a@x.com"}, wantCode: 0, wantOut: "a@x.com is already an admin", wantCall: true, wantGrnt: true},
		{name: "revoke", args: []string{"revoke", "a@x.com"}, setter: fakeAdminSetter{changed: true}, wantCode: 0, wantOut: "revoked admin from a@x.com", wantCall: true},
		{name: "revoke non-admin", args: []string{"revoke", "a@x.com"}, wantCode: 0, wantOut: "a@x.com is not an admin", wantCall: true},
		{name: "unknown email", args: []string{"grant", "no@x.com"}, setter: fakeAdminSetter{err: fmt.Errorf("x: %w", service.ErrNotFound)}, wantCode: 1, wantOut: "no user with email no@x.com", wantCall: true, wantGrnt: true},
		{name: "db error", args: []string{"grant", "a@x.com"}, setter: fakeAdminSetter{err: fmt.Errorf("db down")}, wantCode: 1, wantOut: "db down", wantCall: true, wantGrnt: true},
		{name: "no args", args: nil, wantCode: 2, wantOut: "usage:"},
		{name: "bad verb", args: []string{"promote", "a@x.com"}, wantCode: 2, wantOut: "usage:"},
		{name: "missing email", args: []string{"grant"}, wantCode: 2, wantOut: "usage:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			setter := tc.setter
			code := runAdminCommand(context.Background(), tc.args, &setter, "", &out)
			if code != tc.wantCode || !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("want code %d and %q, got %d %q", tc.wantCode, tc.wantOut, code, out.String())
			}
			if (setter.callCount > 0) != tc.wantCall {
				t.Errorf("SetAdmin called=%v, want %v", setter.callCount > 0, tc.wantCall)
			}
			if tc.wantCall && setter.gotGrant != tc.wantGrnt {
				t.Errorf("grant=%v, want %v", setter.gotGrant, tc.wantGrnt)
			}
		})
	}
}

func TestRunAdminCommand_Create(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		password string
		setter   fakeAdminSetter
		wantCode int
		wantOut  string
	}{
		{name: "new user", args: []string{"create", "a@x.com"}, password: "s3cret-pass", setter: fakeAdminSetter{created: true}, wantCode: 0, wantOut: "created admin a@x.com"},
		{name: "existing user", args: []string{"create", "a@x.com"}, setter: fakeAdminSetter{}, wantCode: 0, wantOut: "a@x.com already exists: granted admin, password unchanged"},
		{name: "weak password", args: []string{"create", "a@x.com"}, password: "short", setter: fakeAdminSetter{err: fmt.Errorf("x: %w", service.ErrWeakPassword)}, wantCode: 1, wantOut: "ADMIN_PASSWORD must be 8-72 bytes"},
		{name: "missing email", args: []string{"create"}, wantCode: 2, wantOut: "usage:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			setter := tc.setter
			code := runAdminCommand(context.Background(), tc.args, &setter, tc.password, &out)
			if code != tc.wantCode || !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("want code %d and %q, got %d %q", tc.wantCode, tc.wantOut, code, out.String())
			}
			if tc.wantCode != 2 && setter.gotPassword != tc.password {
				t.Errorf("password not passed through: %q", setter.gotPassword)
			}
			if strings.Contains(out.String(), "s3cret-pass") {
				t.Error("password echoed to output")
			}
		})
	}
}
