package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/april/shorten-link/internal/repository"
	"github.com/april/shorten-link/internal/service"
)

func mustExec(t *testing.T, db *sqlx.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func auditRows(t *testing.T, db *sqlx.DB, action string) []map[string]any {
	t.Helper()
	rows, err := db.QueryxContext(context.Background(),
		`SELECT user_id, resource_type, resource_id, CAST(metadata AS CHAR) AS metadata FROM audit_log WHERE action = ? ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var userID, resourceID int64
		var resourceType string
		var meta *string
		if err := rows.Scan(&userID, &resourceType, &resourceID, &meta); err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		if meta != nil {
			json.Unmarshal([]byte(*meta), &m) //nolint:errcheck
		}
		out = append(out, map[string]any{"user_id": userID, "resource_type": resourceType, "resource_id": resourceID, "meta": m})
	}
	return out
}

func TestAdminMySQL_ListUsers(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewAdminMySQL(db)
	ctx := context.Background()

	for i := 1; i <= 25; i++ {
		mustExec(t, db, `INSERT INTO users (id, email, password_hash) VALUES (?, ?, 'x')`, i, fmt.Sprintf("u%d@x.com", i))
	}
	mustExec(t, db, `UPDATE users SET email = 'match@b.co' WHERE id = 3`)
	mustExec(t, db, `UPDATE users SET disabled_at = UTC_TIMESTAMP() WHERE id IN (4, 5)`)
	mustExec(t, db, `INSERT INTO links (id, user_id, short_code, original_url) VALUES (100, 3, 'a', 'https://e.com'), (101, 3, 'b', 'https://e.com')`)

	page1, err := repo.ListUsers(ctx, service.UserFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 20 || page1[0].ID != 25 || page1[19].ID != 6 {
		t.Fatalf("page 1: want 20 rows 25..6, got %d rows, first %d", len(page1), page1[0].ID)
	}
	page2, _ := repo.ListUsers(ctx, service.UserFilter{BeforeID: page1[19].ID})
	if len(page2) != 5 || page2[0].ID != 5 {
		t.Fatalf("page 2: want 5 rows from 5, got %d", len(page2))
	}

	got, _ := repo.ListUsers(ctx, service.UserFilter{Query: "b.co"})
	if len(got) != 1 || got[0].ID != 3 || got[0].LinkCount != 2 || got[0].Role != service.RoleUser {
		t.Errorf("query: want user 3 with 2 links, got %+v", got)
	}
	got, _ = repo.ListUsers(ctx, service.UserFilter{Query: "%"})
	if len(got) != 0 {
		t.Errorf("query %%: LIKE wildcard must be escaped, got %d rows", len(got))
	}
	got, _ = repo.ListUsers(ctx, service.UserFilter{Status: "disabled"})
	if len(got) != 2 || got[0].ID != 5 || got[0].DisabledAt == nil {
		t.Errorf("disabled: want users 5,4, got %+v", got)
	}
	got, _ = repo.ListUsers(ctx, service.UserFilter{Status: "active"})
	if len(got) != 20 {
		t.Errorf("active: want a full page, got %d", len(got))
	}
	for _, u := range got {
		if u.DisabledAt != nil {
			t.Errorf("active list contains disabled user %d", u.ID)
		}
	}
}

func TestAdminMySQL_UserWrites(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewAdminMySQL(db)
	ctx := context.Background()

	mustExec(t, db, `INSERT INTO users (id, email, password_hash) VALUES (1, 'admin@x.com', 'x'), (2, 'u@x.com', 'x')`)
	mustExec(t, db, `INSERT INTO links (id, user_id, short_code, original_url, clicks) VALUES (10, 2, 'old', 'https://e.com', 5), (20, 2, 'new', 'https://e.com', 7)`)

	u, err := repo.GetUser(ctx, 2, 20)
	if err != nil || u.LinksToday != 1 || u.TotalClicks != 12 || u.LinkCount != 2 {
		t.Fatalf("GetUser: want today=1 clicks=12 links=2, got %+v, %v", u, err)
	}
	if _, err := repo.GetUser(ctx, 999, 0); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("GetUser unknown: want ErrNotFound, got %v", err)
	}

	changed, err := repo.SetUserDisabled(ctx, 1, 2, true, "spam")
	if err != nil || !changed {
		t.Fatalf("disable: changed=%v err=%v", changed, err)
	}
	changed, err = repo.SetUserDisabled(ctx, 1, 2, true, "again")
	if err != nil || changed {
		t.Fatalf("disable twice: want changed=false, got %v %v", changed, err)
	}
	rows := auditRows(t, db, "user.disable")
	if len(rows) != 1 || rows[0]["user_id"] != int64(1) || rows[0]["resource_type"] != "user" ||
		rows[0]["resource_id"] != int64(2) || rows[0]["meta"].(map[string]any)["reason"] != "spam" {
		t.Errorf("want one user.disable audit row with reason, got %v", rows)
	}
	if changed, err := repo.SetUserDisabled(ctx, 1, 2, false, ""); err != nil || !changed {
		t.Fatalf("enable: changed=%v err=%v", changed, err)
	}
	rows = auditRows(t, db, "user.enable")
	if len(rows) != 1 {
		t.Errorf("want one user.enable row, got %v", rows)
	}
	if _, ok := rows[0]["meta"].(map[string]any)["reason"]; ok {
		t.Error("empty reason must not be stored")
	}
	if _, err := repo.SetUserDisabled(ctx, 1, 999, true, ""); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("disable unknown: want ErrNotFound, got %v", err)
	}

	if err := repo.SetUserQuota(ctx, 1, 2, 5, "trial"); err != nil {
		t.Fatal(err)
	}
	rows = auditRows(t, db, "user.quota")
	meta := rows[0]["meta"].(map[string]any)
	if len(rows) != 1 || meta["before"] != float64(100) || meta["after"] != float64(5) || meta["reason"] != "trial" {
		t.Errorf("want quota audit before=100 after=5, got %v", rows)
	}
	if u, _ := repo.GetUser(ctx, 2, 0); u.QuotaDaily != 5 {
		t.Errorf("quota not saved: %d", u.QuotaDaily)
	}
	if err := repo.SetUserQuota(ctx, 1, 2, 5, "same again"); err != nil {
		t.Fatal(err)
	}
	if n := len(auditRows(t, db, "user.quota")); n != 1 {
		t.Errorf("setting the same quota must not write an audit row, got %d rows", n)
	}
	if err := repo.SetUserQuota(ctx, 1, 999, 5, ""); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("quota unknown: want ErrNotFound, got %v", err)
	}

	codes, err := repo.LinkCodesByUser(ctx, 2)
	if err != nil || len(codes) != 2 {
		t.Errorf("LinkCodesByUser: want 2 codes, got %v, %v", codes, err)
	}

	if changed, err := repo.SetRole(ctx, 0, "U@x.com", service.RoleAdmin); err != nil || !changed {
		t.Fatalf("grant: changed=%v err=%v", changed, err)
	}
	if changed, _ := repo.SetRole(ctx, 0, "u@x.com", service.RoleAdmin); changed {
		t.Error("grant twice: want changed=false")
	}
	rows = auditRows(t, db, "admin.grant")
	if len(rows) != 1 || rows[0]["user_id"] != int64(0) || rows[0]["resource_id"] != int64(2) {
		t.Errorf("want one admin.grant row by system, got %v", rows)
	}
	if _, err := repo.SetRole(ctx, 0, "nobody@x.com", service.RoleAdmin); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("grant unknown: want ErrNotFound, got %v", err)
	}
	if changed, _ := repo.SetRole(ctx, 0, "u@x.com", service.RoleUser); !changed || len(auditRows(t, db, "admin.revoke")) != 1 {
		t.Error("revoke: want changed and one admin.revoke row")
	}
}

func TestAdminMySQL_ListLinks(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewAdminMySQL(db)
	ctx := context.Background()

	mustExec(t, db, `INSERT INTO users (id, email, password_hash) VALUES (1, 'a@x.com', 'x'), (2, 'bad@x.com', 'x')`)
	mustExec(t, db, `UPDATE users SET disabled_at = UTC_TIMESTAMP() WHERE id = 2`)
	mustExec(t, db, `INSERT INTO links (id, user_id, short_code, original_url, expires_at, disabled_at) VALUES
		(10, 1, 'live',    'https://good.com/a', NULL, NULL),
		(11, 1, 'off',     'https://evil.com/x', NULL, UTC_TIMESTAMP()),
		(12, 1, 'gone',    'https://good.com/b', UTC_TIMESTAMP() - INTERVAL 1 DAY, NULL),
		(13, 2, 'ownerx',  'https://good.com/c', NULL, NULL),
		(14, 1, 'future',  'https://good.com/d', UTC_TIMESTAMP() + INTERVAL 1 DAY, NULL)`)

	ids := func(f service.LinkFilter) []int64 {
		t.Helper()
		links, err := repo.ListLinks(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out := []int64{}
		for _, l := range links {
			out = append(out, l.ID)
		}
		return out
	}
	check := func(name string, got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: want %v, got %v", name, want, got)
		}
	}
	check("all newest first", ids(service.LinkFilter{}), 14, 13, 12, 11, 10)
	check("active", ids(service.LinkFilter{Status: "active"}), 14, 10)
	check("disabled", ids(service.LinkFilter{Status: "disabled"}), 13, 11)
	check("expired", ids(service.LinkFilter{Status: "expired"}), 12)
	check("code exact", ids(service.LinkFilter{Query: "off"}), 11)
	check("url contains", ids(service.LinkFilter{Query: "good.com/"}), 14, 13, 12, 10)
	check("user", ids(service.LinkFilter{UserID: 2}), 13)
	check("before", ids(service.LinkFilter{BeforeID: 12}), 11, 10)

	l, err := repo.GetLink(ctx, "ownerx")
	if err != nil || l.OwnerEmail != "bad@x.com" || l.OwnerDisabledAt == nil || l.OwnerID != 2 {
		t.Errorf("GetLink: want owner bad@x.com disabled, got %+v, %v", l, err)
	}
	if _, err := repo.GetLink(ctx, "nope"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("GetLink unknown: want ErrNotFound, got %v", err)
	}
}

func TestAdminMySQL_LinkWrites(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewAdminMySQL(db)
	ctx := context.Background()

	mustExec(t, db, `INSERT INTO users (id, email, password_hash) VALUES (1, 'admin@x.com', 'x')`)
	mustExec(t, db, `INSERT INTO links (id, user_id, short_code, original_url) VALUES (10, 1, 'abc', 'https://e.com'), (11, 1, 'del', 'https://e.com')`)

	if changed, err := repo.SetLinkDisabled(ctx, 1, "abc", true, "phishing"); err != nil || !changed {
		t.Fatalf("disable: %v %v", changed, err)
	}
	if changed, _ := repo.SetLinkDisabled(ctx, 1, "abc", true, ""); changed {
		t.Error("disable twice: want changed=false")
	}
	rows := auditRows(t, db, "link.disable")
	meta := rows[0]["meta"].(map[string]any)
	if len(rows) != 1 || rows[0]["resource_type"] != "link" || rows[0]["resource_id"] != int64(10) ||
		meta["code"] != "abc" || meta["reason"] != "phishing" {
		t.Errorf("want one link.disable row, got %v", rows)
	}
	if changed, _ := repo.SetLinkDisabled(ctx, 1, "abc", false, ""); !changed || len(auditRows(t, db, "link.enable")) != 1 {
		t.Error("enable: want changed and one link.enable row")
	}
	if _, err := repo.SetLinkDisabled(ctx, 1, "nope", true, ""); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("disable unknown: want ErrNotFound, got %v", err)
	}

	if err := repo.DeleteLink(ctx, 1, "del", "dup"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetLink(ctx, "del"); !errors.Is(err, service.ErrNotFound) {
		t.Error("link still exists after delete")
	}
	rows = auditRows(t, db, "link.delete")
	if len(rows) != 1 || rows[0]["resource_id"] != int64(11) || rows[0]["meta"].(map[string]any)["code"] != "del" {
		t.Errorf("want one link.delete row, got %v", rows)
	}
	if err := repo.DeleteLink(ctx, 1, "nope", ""); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("delete unknown: want ErrNotFound, got %v", err)
	}
	if len(auditRows(t, db, "link.delete")) != 1 {
		t.Error("failed delete wrote an audit row")
	}
}

func TestAdminMySQL_ListAudit(t *testing.T) {
	db := setupMySQL(t)
	repo := repository.NewAdminMySQL(db)
	ctx := context.Background()

	mustExec(t, db, `INSERT INTO users (id, email, password_hash) VALUES (1, 'admin@x.com', 'x'), (2, 'u@x.com', 'x')`)
	mustExec(t, db, `INSERT INTO links (id, user_id, short_code, original_url) VALUES (10, 2, 'abc', 'https://e.com')`)
	repo.SetRole(ctx, 0, "admin@x.com", service.RoleAdmin) //nolint:errcheck
	repo.SetUserDisabled(ctx, 1, 2, true, "spam")          //nolint:errcheck
	repo.SetLinkDisabled(ctx, 1, "abc", true, "")          //nolint:errcheck

	all, err := repo.ListAudit(ctx, service.AuditFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("want 3 entries, got %d, %v", len(all), err)
	}
	if all[0].Action != "link.disable" || all[0].ActorEmail != "admin@x.com" || all[0].ResourceID != 10 {
		t.Errorf("newest first with actor email: got %+v", all[0])
	}
	if all[2].ActorID != 0 || all[2].ActorEmail != "" || all[2].Action != "admin.grant" {
		t.Errorf("system entry: got %+v", all[2])
	}
	var meta map[string]any
	if err := json.Unmarshal(all[1].Metadata, &meta); err != nil || meta["reason"] != "spam" {
		t.Errorf("metadata: got %s, %v", all[1].Metadata, err)
	}

	users, _ := repo.ListAudit(ctx, service.AuditFilter{ResourceType: "user"})
	if len(users) != 2 {
		t.Errorf("resource_type=user: want 2, got %d", len(users))
	}
	byAdmin, _ := repo.ListAudit(ctx, service.AuditFilter{ActorID: 1})
	if len(byAdmin) != 2 {
		t.Errorf("actor 1: want 2, got %d", len(byAdmin))
	}
	older, _ := repo.ListAudit(ctx, service.AuditFilter{BeforeID: all[0].ID})
	if len(older) != 2 {
		t.Errorf("before: want 2, got %d", len(older))
	}
}
