package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/model"
)

const (
	owner    = uint(1)
	stranger = uint(2)
)

func quizOf(hostID uint, isPublic, allowEdit bool) model.Quiz {
	return model.Quiz{ID: 7, HostID: hostID, IsPublic: isPublic, AllowEdit: allowEdit}
}

// The owner keeps every right no matter what the flags say — including on a
// quiz they never published.
func TestRightsOnOwnerAlwaysHasEveryRight(t *testing.T) {
	for _, q := range []model.Quiz{
		quizOf(owner, false, false),
		quizOf(owner, true, false),
		quizOf(owner, true, true),
	} {
		got := rightsOn(&q, owner)
		want := quizRights{Owner: true, View: true, Host: true, Copy: true, Edit: true}
		if got != want {
			t.Fatalf("owner of quiz(public=%v, allowEdit=%v) got %+v, want %+v",
				q.IsPublic, q.AllowEdit, got, want)
		}
	}
}

func TestRightsOnPrivateQuizGrantsNothingToOthers(t *testing.T) {
	q := quizOf(owner, false, false)
	if got := rightsOn(&q, stranger); got != (quizRights{}) {
		t.Fatalf("a private quiz leaked rights to a stranger: %+v", got)
	}
}

// Publishing alone gives reading, hosting and copying — editing is a separate
// decision the owner has to make on purpose.
func TestRightsOnPublicQuizGrantsViewHostAndCopyButNotEdit(t *testing.T) {
	q := quizOf(owner, true, false)
	got := rightsOn(&q, stranger)
	want := quizRights{View: true, Host: true, Copy: true}
	if got != want {
		t.Fatalf("public quiz gave a stranger %+v, want %+v", got, want)
	}
}

func TestRightsOnAllowEditOpensTheOriginal(t *testing.T) {
	q := quizOf(owner, true, true)
	got := rightsOn(&q, stranger)
	if !got.Edit {
		t.Fatalf("allow_edit did not grant editing: %+v", got)
	}
	if !got.Copy {
		t.Fatal("an editable shared quiz must still be copyable")
	}
}

// allow_edit must never act on its own. A row unshared while the flag was set
// is private again, and private means no rights at all.
func TestRightsOnIgnoresAllowEditWithoutIsPublic(t *testing.T) {
	q := quizOf(owner, false, true)
	if got := rightsOn(&q, stranger); got != (quizRights{}) {
		t.Fatalf("allow_edit granted %+v on a private quiz", got)
	}
}

// Copying is what a reader does instead of editing, so it has to be granted
// wherever viewing is — otherwise the read-only screen has no way out.
func TestRightsOnGrantsCopyWhereverItGrantsView(t *testing.T) {
	for _, q := range []model.Quiz{
		quizOf(owner, true, false),
		quizOf(owner, true, true),
		quizOf(owner, false, false),
	} {
		got := rightsOn(&q, stranger)
		if got.View != got.Copy {
			t.Fatalf("quiz(public=%v) gave view=%v but copy=%v",
				q.IsPublic, got.View, got.Copy)
		}
	}
}

// Deleting, and changing the sharing flags, are both gated on Owner — so no
// combination of flags may ever set it.
func TestRightsOnNeverMakesAStrangerTheOwner(t *testing.T) {
	for _, q := range []model.Quiz{
		quizOf(owner, true, false),
		quizOf(owner, true, true),
	} {
		if rightsOn(&q, stranger).Owner {
			t.Fatalf("quiz(public=%v, allowEdit=%v) made a stranger the owner",
				q.IsPublic, q.AllowEdit)
		}
	}
}

func TestRightsOnHandlesAMissingQuiz(t *testing.T) {
	if got := rightsOn(nil, owner); got != (quizRights{}) {
		t.Fatalf("a nil quiz granted %+v", got)
	}
}

// Unpublishing clears allow_edit, so publishing again later cannot silently
// reopen editing the owner believes they switched off.
func TestNormalizeSharingClearsEditWhenUnpublished(t *testing.T) {
	cases := []struct {
		inPublic, inEdit     bool
		wantPublic, wantEdit bool
	}{
		{false, false, false, false},
		{false, true, false, false},
		{true, false, true, false},
		{true, true, true, true},
	}
	for _, tc := range cases {
		gotPublic, gotEdit := normalizeSharing(tc.inPublic, tc.inEdit)
		if gotPublic != tc.wantPublic || gotEdit != tc.wantEdit {
			t.Fatalf("normalizeSharing(%v, %v) = (%v, %v), want (%v, %v)",
				tc.inPublic, tc.inEdit, gotPublic, gotEdit, tc.wantPublic, tc.wantEdit)
		}
	}
}

// UpdateQuiz binds UpdateQuizReq and writes every field it is given. If a
// sharing flag ever appears in that payload, saving a quiz becomes a way to
// publish one — so assert neither is bindable there.
func TestUpdateQuizReqCannotCarrySharingFlags(t *testing.T) {
	var req UpdateQuizReq
	body := `{"title":"t","is_public":true,"allow_edit":true}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("payload did not bind: %v", err)
	}

	forbidden := map[string]bool{"is_public": true, "allow_edit": true}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if forbidden[name] {
				t.Fatalf("UpdateQuizReq exposes %q — sharing flags must only be "+
					"writable through PATCH /quizzes/:id/sharing", name)
			}
		}
	}
	walk(reflect.TypeOf(req))
}

// Query parsing ---------------------------------------------------------------

func TestParseSharedQuizQueryDefaultsAndClamps(t *testing.T) {
	cases := map[string]struct {
		page, size         string
		wantPage, wantSize int
	}{
		"empty":             {"", "", 1, sharedQuizDefaultPageSize},
		"garbage":           {"abc", "abc", 1, sharedQuizDefaultPageSize},
		"page below one":    {"0", "", 1, sharedQuizDefaultPageSize},
		"negative page":     {"-3", "", 1, sharedQuizDefaultPageSize},
		"size over the cap": {"", "5000", 1, sharedQuizMaxPageSize},
		"size below one":    {"", "0", 1, sharedQuizDefaultPageSize},
		"valid":             {"3", "10", 3, 10},
		"padded":            {" 3 ", " 10 ", 3, 10},
	}
	for name, tc := range cases {
		got := parseSharedQuizQuery("", tc.page, tc.size)
		if got.Page != tc.wantPage || got.PageSize != tc.wantSize {
			t.Fatalf("%s: got page=%d size=%d, want page=%d size=%d",
				name, got.Page, got.PageSize, tc.wantPage, tc.wantSize)
		}
	}
}

func TestParseSharedQuizQueryOffset(t *testing.T) {
	q := parseSharedQuizQuery("", "3", "10")
	if q.Offset() != 20 {
		t.Fatalf("page 3 of 10 should start at row 20, got %d", q.Offset())
	}
	first := parseSharedQuizQuery("", "1", "10")
	if first.Offset() != 0 {
		t.Fatalf("the first page must not skip rows, got %d", first.Offset())
	}
}

func TestParseSharedQuizQueryBoundsTheSearchTerm(t *testing.T) {
	long := strings.Repeat("a", sharedQuizMaxSearchLen+50)
	got := parseSharedQuizQuery(long, "", "")
	if len(got.Search) != sharedQuizMaxSearchLen {
		t.Fatalf("search term was not truncated: %d chars", len(got.Search))
	}
	if blank := parseSharedQuizQuery("   ", "", ""); blank.Search != "" {
		t.Fatalf("whitespace should mean no filter, got %q", blank.Search)
	}
}

// A search for "100%" must match titles containing "100%", not every row.
func TestEscapeLikeNeutralisesWildcards(t *testing.T) {
	cases := map[string]string{
		"100%":    `100\%`,
		"a_b":     `a\_b`,
		`back\up`: `back\\up`,
		"plain":   "plain",
		`%_\`:     `\%\_\\`,
		"":        "",
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Fatalf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSharedQuizQueryPatternWrapsTheEscapedTerm(t *testing.T) {
	q := parseSharedQuizQuery("100%", "", "")
	if want := `%100\%%`; q.Pattern() != want {
		t.Fatalf("Pattern() = %q, want %q", q.Pattern(), want)
	}
}

// The listing must not turn an author with no nickname into a blank byline, and
// must never fall back to something identifying like the email.
func TestAuthorDisplayNameFallsBackWhenNicknameIsBlank(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		if got := authorDisplayName(in); strings.TrimSpace(got) == "" {
			t.Fatalf("blank nickname %q produced an empty byline", in)
		}
	}
	if got := authorDisplayName("  Mai  "); got != "Mai" {
		t.Fatalf("nickname was not trimmed: %q", got)
	}
}

// sharedQuizItem is what leaves the server for every logged-in host. Assert the
// author's identifying columns are not on it — model.Quiz carries host_id, and
// preloading the author would carry their email.
func TestSharedQuizItemDoesNotLeakTheAuthorsIdentity(t *testing.T) {
	leaky := map[string]bool{"host_id": true, "email": true, "host": true, "author": true}
	rt := reflect.TypeOf(sharedQuizItem{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if leaky[name] {
			t.Fatalf("sharedQuizItem exposes %q to every host on the platform", name)
		}
	}
}

// Rejection shape ------------------------------------------------------------

func denyOf(t *testing.T, r quizRights) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/quizzes/7", strings.NewReader("{}"))

	denyQuizAccess(c, r)

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	msg, _ := body["error"].(string)
	return rec.Code, msg
}

// A quiz the caller cannot see must come back as 404. A 403 would confirm the
// id exists, which turns the endpoint into an enumeration oracle for quizzes
// their owners chose not to publish.
func TestDenyQuizAccessHidesQuizzesTheCallerCannotSee(t *testing.T) {
	code, msg := denyOf(t, quizRights{})
	if code != http.StatusNotFound {
		t.Fatalf("an invisible quiz answered %d, want 404", code)
	}
	if msg != "Quiz not found" {
		t.Fatalf("unexpected message %q — it must match the catalog entry", msg)
	}
}

// One they can see but not change gets an honest 403, and the message has to
// name the way forward: on a shared quiz that is always "duplicate it".
func TestDenyQuizAccessPointsAReaderAtDuplicating(t *testing.T) {
	code, msg := denyOf(t, quizRights{View: true, Host: true, Copy: true})
	if code != http.StatusForbidden {
		t.Fatalf("a read-only shared quiz answered %d, want 403", code)
	}
	if msg != "This shared quiz is read-only — duplicate it to make changes" {
		t.Fatalf("unexpected message %q — it must match the catalog entry", msg)
	}
}

// Game mode ------------------------------------------------------------------
//
// These cover the defect that made a shared quiz unhostable: the mode used to
// be saved onto the quiz just before the room was created, so starting a game
// needed write access to somebody else's quiz. It belongs to the room.

func TestNormalizeGameMode(t *testing.T) {
	cases := map[string]string{
		"solo":         "player_paced",
		"player_paced": "player_paced",
		"  SOLO  ":     "player_paced",
		"classic":      "host_paced",
		"host_paced":   "host_paced",
		"":             "host_paced",
		// Gameplay compares against "player_paced" exactly, so anything
		// unrecognised has to land on the host-paced default rather than be
		// passed through and silently match nothing.
		"nonsense": "host_paced",
	}
	for in, want := range cases {
		if got := normalizeGameMode(in); got != want {
			t.Fatalf("normalizeGameMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// The mode has to be merged in, not written over the top: theme_config also
// holds the colours, the background and explanation_duration, and the room
// reads its own copy of every one of them.
func TestWithGameModeKeepsTheRestOfTheTheme(t *testing.T) {
	in := `{"primary_color":"#6d28d9","explanation_duration":8,"game_mode":"host_paced"}`
	out, err := withGameMode(in, "player_paced")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got["game_mode"] != "player_paced" {
		t.Fatalf("game_mode not applied: %v", got["game_mode"])
	}
	if got["primary_color"] != "#6d28d9" {
		t.Fatalf("colour was dropped: %v", got["primary_color"])
	}
	if got["explanation_duration"] != float64(8) {
		t.Fatalf("explanation_duration was dropped: %v", got["explanation_duration"])
	}
}

func TestWithGameModeHandlesEmptyAndBrokenThemes(t *testing.T) {
	// A quiz with no theme, or one whose theme cannot be parsed, still has to
	// be playable — the mode is what the room needs, the rest is decoration.
	for _, in := range []string{"", "   ", "not json", "[1,2,3]"} {
		out, err := withGameMode(in, "player_paced")
		if err != nil {
			t.Fatalf("withGameMode(%q) errored: %v", in, err)
		}
		var got map[string]interface{}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("withGameMode(%q) produced invalid JSON %q", in, out)
		}
		if got["game_mode"] != "player_paced" {
			t.Fatalf("withGameMode(%q) lost the mode: %q", in, out)
		}
	}
}

// Picking a mode for one game must not change what the quiz is saved as —
// that is what made hosting a write, and what let one host's Solo game flip
// the author's quiz for everybody else.
func TestWithGameModeDoesNotMutateTheQuizsOwnTheme(t *testing.T) {
	quizTheme := `{"primary_color":"#111111","game_mode":"host_paced"}`
	if _, err := withGameMode(quizTheme, "player_paced"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if quizTheme != `{"primary_color":"#111111","game_mode":"host_paced"}` {
		t.Fatalf("the quiz's own theme string was modified: %s", quizTheme)
	}
}
