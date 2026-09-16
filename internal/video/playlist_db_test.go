package video

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sendrec/sendrec/internal/auth"
)

// withURLParam injects a chi route param without standing up a router.
func withURLParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// seedMember adds a second person to an existing workspace.
func seedMember(t *testing.T, pool *pgxpool.Pool, orgID, email, role string) string {
	t.Helper()
	ctx := context.Background()

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password, name) VALUES ($1, 'x', 'Member')
		 ON CONFLICT (email) DO UPDATE SET name = EXCLUDED.name RETURNING id`, email,
	).Scan(&userID); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO organization_members (organization_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT (organization_id, user_id) DO UPDATE SET role = EXCLUDED.role`, orgID, userID, role,
	); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
	return userID
}

// Playlist scoping is an access-control rule expressed in SQL, so these run
// against a real database. They skip when TEST_DATABASE_URL is unset.
//
// Seeded users are keyed on the test name, so a repeat run reuses them; clearing
// playlists keeps the list assertions counting only what the run created.
func playlistHandler(t *testing.T, pool *pgxpool.Pool) *Handler {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM playlists`); err != nil {
		t.Fatalf("reset playlists: %v", err)
	}
	return newPlaylistHandler(pool)
}

func newPlaylistHandler(pool *pgxpool.Pool) *Handler {
	return NewHandler(pool, &mockStorage{}, "https://app.sendrec.eu", 0, 0, 0, 0, "secret", false)
}

func playlistRequest(t *testing.T, method, body, userID, orgID, role string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "/api/playlists", strings.NewReader(body))
	ctx := auth.ContextWithUserID(req.Context(), userID)
	if orgID != "" {
		ctx = auth.ContextWithOrg(ctx, orgID, role)
	}
	return req.WithContext(ctx)
}

func createPlaylist(t *testing.T, h *Handler, title, userID, orgID, role string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.CreatePlaylist(rec, playlistRequest(t, http.MethodPost, `{"title":"`+title+`"}`, userID, orgID, role))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create playlist %q: status=%d body=%s", title, rec.Code, rec.Body.String())
	}
	var item playlistItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode created playlist: %v", err)
	}
	return item.ID
}

func listPlaylistTitles(t *testing.T, h *Handler, userID, orgID, role string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ListPlaylists(rec, playlistRequest(t, http.MethodGet, "", userID, orgID, role))
	if rec.Code != http.StatusOK {
		t.Fatalf("list playlists: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var items []playlistItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode playlists: %v", err)
	}
	titles := make([]string, 0, len(items))
	for _, item := range items {
		titles = append(titles, item.Title)
	}
	return titles
}

func TestPlaylistDB_WorkspaceAndPersonalListsStaySeparate(t *testing.T) {
	pool := brandingTestDB(t)
	userID, orgID := seedUserAndOrg(t, pool)
	h := playlistHandler(t, pool)

	createPlaylist(t, h, "Personal list", userID, "", "")
	createPlaylist(t, h, "Workspace list", userID, orgID, "owner")

	personal := listPlaylistTitles(t, h, userID, "", "")
	if len(personal) != 1 || personal[0] != "Personal list" {
		t.Errorf("personal context should see only the personal playlist, got %v", personal)
	}

	workspace := listPlaylistTitles(t, h, userID, orgID, "owner")
	if len(workspace) != 1 || workspace[0] != "Workspace list" {
		t.Errorf("workspace context should see only the workspace playlist, got %v", workspace)
	}
}

// A workspace playlist belongs to the workspace, so an owner reaches one a
// different member created — the rule videos already follow.
func TestPlaylistDB_OwnerReachesAnotherMembersWorkspacePlaylist(t *testing.T) {
	pool := brandingTestDB(t)
	ownerID, orgID := seedUserAndOrg(t, pool)
	memberID := seedMember(t, pool, orgID, "member@example.com", "member")
	h := playlistHandler(t, pool)

	playlistID := createPlaylist(t, h, "Member list", memberID, orgID, "member")

	rec := httptest.NewRecorder()
	req := playlistRequest(t, http.MethodDelete, "", ownerID, orgID, "owner")
	h.DeletePlaylist(rec, withURLParam(req, "id", playlistID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner deleting a workspace playlist: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// Personal playlists stay private to their owner even inside a shared workspace.
func TestPlaylistDB_WorkspaceContextCannotReachPersonalPlaylist(t *testing.T) {
	pool := brandingTestDB(t)
	userID, orgID := seedUserAndOrg(t, pool)
	otherID := seedMember(t, pool, orgID, "other@example.com", "owner")
	h := playlistHandler(t, pool)

	playlistID := createPlaylist(t, h, "Private list", userID, "", "")

	rec := httptest.NewRecorder()
	req := playlistRequest(t, http.MethodDelete, "", otherID, orgID, "owner")
	h.DeletePlaylist(rec, withURLParam(req, "id", playlistID))
	if rec.Code == http.StatusNoContent {
		t.Error("a workspace owner deleted someone's personal playlist")
	}
}
