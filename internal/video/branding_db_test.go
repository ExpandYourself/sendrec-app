package video

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sendrec/sendrec/internal/auth"
	"github.com/sendrec/sendrec/internal/database"
)

// Branding is stored in a table whose shape the mocked tests cannot see: the
// org-scoped statements only fail against the real constraints. These tests run
// when TEST_DATABASE_URL points at a disposable Postgres, and skip otherwise.
func brandingTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	db, err := database.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(db.Close)

	// Each test starts from an empty branding table; the rest of the schema is
	// shared and only read through the rows this test inserts.
	if _, err := db.Pool.Exec(context.Background(), `DELETE FROM user_branding`); err != nil {
		t.Fatalf("reset user_branding: %v", err)
	}

	return db.Pool
}

func seedUserAndOrg(t *testing.T, pool *pgxpool.Pool) (userID, orgID string) {
	t.Helper()
	ctx := context.Background()

	email := "branding-" + strings.ReplaceAll(t.Name(), "/", "-") + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password, name) VALUES ($1, 'x', 'Owner')
		 ON CONFLICT (email) DO UPDATE SET name = EXCLUDED.name RETURNING id`, email,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	slug := "org-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ('Acme', $1)
		 ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name RETURNING id`, slug,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO organization_members (organization_id, user_id, role) VALUES ($1, $2, 'owner')
		 ON CONFLICT DO NOTHING`, orgID, userID,
	); err != nil {
		t.Fatalf("insert membership: %v", err)
	}

	return userID, orgID
}

func brandingRequest(t *testing.T, method, body, userID, orgID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "/api/branding", strings.NewReader(body))
	ctx := auth.ContextWithUserID(req.Context(), userID)
	if orgID != "" {
		ctx = auth.ContextWithOrg(ctx, orgID, "owner")
	}
	return req.WithContext(ctx)
}

func TestBrandingDB_WorkspaceBrandingRoundtrips(t *testing.T) {
	pool := brandingTestDB(t)
	userID, orgID := seedUserAndOrg(t, pool)

	h := NewHandler(pool, &mockStorage{}, "https://app.sendrec.eu", 0, 0, 0, 0, "secret", false)
	h.SetBrandingEnabled(true)

	rec := httptest.NewRecorder()
	h.PutBrandingSettings(rec, brandingRequest(t, http.MethodPut,
		`{"companyName":"Acme Inc","colorAccent":"#ff0000"}`, userID, orgID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("save workspace branding: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.GetBrandingSettings(rec, brandingRequest(t, http.MethodGet, "", userID, orgID))
	if rec.Code != http.StatusOK {
		t.Fatalf("load workspace branding: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got brandingSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CompanyName == nil || *got.CompanyName != "Acme Inc" {
		t.Errorf("company name did not round-trip: %+v", got.CompanyName)
	}
	if got.ColorAccent == nil || *got.ColorAccent != "#ff0000" {
		t.Errorf("accent colour did not round-trip: %+v", got.ColorAccent)
	}
}

func TestBrandingDB_PersonalAndWorkspaceStaySeparate(t *testing.T) {
	pool := brandingTestDB(t)
	userID, orgID := seedUserAndOrg(t, pool)

	h := NewHandler(pool, &mockStorage{}, "https://app.sendrec.eu", 0, 0, 0, 0, "secret", false)
	h.SetBrandingEnabled(true)

	rec := httptest.NewRecorder()
	h.PutBrandingSettings(rec, brandingRequest(t, http.MethodPut,
		`{"companyName":"Personal Co"}`, userID, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("save personal branding: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.PutBrandingSettings(rec, brandingRequest(t, http.MethodPut,
		`{"companyName":"Acme Inc"}`, userID, orgID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("save workspace branding: status=%d body=%s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct{ scope, orgID, want string }{
		{"personal", "", "Personal Co"},
		{"workspace", orgID, "Acme Inc"},
	} {
		rec = httptest.NewRecorder()
		h.GetBrandingSettings(rec, brandingRequest(t, http.MethodGet, "", userID, tc.orgID))
		var got brandingSettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: decode: %v", tc.scope, err)
		}
		if got.CompanyName == nil || *got.CompanyName != tc.want {
			t.Errorf("%s branding: want %q, got %+v", tc.scope, tc.want, got.CompanyName)
		}
	}
}

// Saving twice must update the workspace's row rather than add a second one.
func TestBrandingDB_WorkspaceBrandingUpsertsSingleRow(t *testing.T) {
	pool := brandingTestDB(t)
	userID, orgID := seedUserAndOrg(t, pool)

	h := NewHandler(pool, &mockStorage{}, "https://app.sendrec.eu", 0, 0, 0, 0, "secret", false)
	h.SetBrandingEnabled(true)

	for _, name := range []string{"First", "Second"} {
		rec := httptest.NewRecorder()
		h.PutBrandingSettings(rec, brandingRequest(t, http.MethodPut,
			`{"companyName":"`+name+`"}`, userID, orgID))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("save %q: status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_branding WHERE organization_id = $1`, orgID,
	).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("expected exactly 1 workspace branding row, got %d", rows)
	}
}
