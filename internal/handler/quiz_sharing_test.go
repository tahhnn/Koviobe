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
	editor   = uint(3)
)

func quizOf(hostID uint) model.Quiz {
	return model.Quiz{ID: 7, HostID: hostID}
}

func grantFor(quizID, uid uint) *model.QuizCollaborator {
	id := uid
	return &model.QuizCollaborator{QuizID: quizID, UserID: &id, Email: "e@x.vn", Role: model.CollaboratorRoleEditor}
}

func TestRightsOnOwnerAlwaysHasEveryRight(t *testing.T) {
	q := quizOf(owner)
	want := quizRights{Owner: true, View: true, Host: true, Copy: true, Edit: true}
	for _, canShare := range []bool{true, false} {
		if got := rightsOn(&q, owner, nil, canShare); got != want {
			t.Fatalf("owner (canShare=%v) got %+v, want %+v", canShare, got, want)
		}
	}
}

// The leak this replaced: the retired is_public flag opened a quiz to every
// account. Whatever the old columns hold, a stranger gets nothing.
func TestRightsOnIgnoresTheRetiredPublicFlags(t *testing.T) {
	q := model.Quiz{ID: 7, HostID: owner, IsPublic: true, AllowEdit: true}
	if got := rightsOn(&q, stranger, nil, true); got != (quizRights{}) {
		t.Fatalf("a public-flagged quiz leaked %+v to a stranger", got)
	}
}

func TestRightsOnEditorMayViewAndEditOnly(t *testing.T) {
	q := quizOf(owner)
	got := rightsOn(&q, editor, grantFor(q.ID, editor), true)
	want := quizRights{View: true, Edit: true}
	if got != want {
		t.Fatalf("editor got %+v, want %+v", got, want)
	}
}

// Not hosting (the room would be the editor's), not copying (a copy outlives
// the grant), and never owner — owner is what gates delete and sharing.
func TestRightsOnEditorCannotHostCopyOrOwn(t *testing.T) {
	q := quizOf(owner)
	got := rightsOn(&q, editor, grantFor(q.ID, editor), true)
	if got.Host || got.Copy || got.Owner {
		t.Fatalf("editor got owner-only rights: %+v", got)
	}
}

func TestRightsOnRejectsAGrantThatIsNotTheCallers(t *testing.T) {
	q := quizOf(owner)
	cases := map[string]*model.QuizCollaborator{
		"another user's grant": grantFor(q.ID, stranger),
		"another quiz's grant": grantFor(q.ID+1, editor),
		"pending invite":       {QuizID: q.ID, Email: "e@x.vn", Role: model.CollaboratorRoleEditor},
		"unknown role":         {QuizID: q.ID, UserID: grantFor(0, editor).UserID, Role: "admin"},
		"no grant":             nil,
	}
	for name, g := range cases {
		if got := rightsOn(&q, editor, g, true); got != (quizRights{}) {
			t.Fatalf("%s granted %+v", name, got)
		}
	}
}

// Sharing is the owner's Pro feature: when their plan lapses the grant is
// suspended, not honoured.
func TestRightsOnSuspendsEditorsWhenOwnerCannotShare(t *testing.T) {
	q := quizOf(owner)
	if got := rightsOn(&q, editor, grantFor(q.ID, editor), false); got != (quizRights{}) {
		t.Fatalf("editor kept %+v after the owner lost sharing", got)
	}
}

func TestRightsOnHandlesAMissingQuiz(t *testing.T) {
	if got := rightsOn(nil, owner, nil, true); got != (quizRights{}) {
		t.Fatalf("a nil quiz granted %+v", got)
	}
}

// UpdateQuiz binds UpdateQuizReq and writes what it is given. If anything about
// sharing ever appears in that payload, an editor's save becomes a way to grant
// access onward.
func TestUpdateQuizReqCannotCarrySharing(t *testing.T) {
	forbidden := map[string]bool{
		"is_public": true, "allow_edit": true, "host_id": true,
		"collaborators": true, "email": true,
	}
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
				t.Fatalf("UpdateQuizReq exposes %q", name)
			}
		}
	}
	walk(reflect.TypeOf(UpdateQuizReq{}))
}

// The retired flags must not reach clients through model.Quiz either:
// UpdateQuiz serialises the model directly.
func TestQuizModelNoLongerSerialisesSharingFlags(t *testing.T) {
	b, err := json.Marshal(model.Quiz{IsPublic: true, AllowEdit: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"is_public", "allow_edit"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Fatalf("model.Quiz still serialises %q: %s", k, b)
		}
	}
}

// Invited addresses ------------------------------------------------------------

func TestNormalizeCollaboratorEmail(t *testing.T) {
	ok := map[string]string{
		"a@b.vn":             "a@b.vn",
		"  Mai@Example.COM ": "mai@example.com",
	}
	for in, want := range ok {
		got, valid := normalizeCollaboratorEmail(in)
		if !valid || got != want {
			t.Fatalf("normalizeCollaboratorEmail(%q) = %q,%v, want %q", in, got, valid, want)
		}
	}
	for _, bad := range []string{
		"", "   ", "not-an-email", "a@b.vn, c@d.vn", "Mai <mai@b.vn>",
		"a@b.vn\r\nBcc: x@y.vn", strings.Repeat("a", 251) + "@b.vn",
	} {
		if got, valid := normalizeCollaboratorEmail(bad); valid {
			t.Fatalf("normalizeCollaboratorEmail(%q) accepted %q", bad, got)
		}
	}
}

// The invite renders the owner's nickname and quiz title — both typed by a
// user — into HTML that lands in someone else's inbox.
func TestShareInviteHTMLEscapesUserText(t *testing.T) {
	out := shareInviteHTML(`<script>x</script>`, 7, `"><img src=x>`, true)
	if strings.Contains(out, "<script>") || strings.Contains(out, `"><img`) {
		t.Fatalf("invite HTML is not escaped: %s", out)
	}
}

func TestToCollaboratorItemStatus(t *testing.T) {
	id := uint(9)
	if s := toCollaboratorItem(model.QuizCollaborator{UserID: &id}).Status; s != "active" {
		t.Fatalf("linked grant reported %q", s)
	}
	if s := toCollaboratorItem(model.QuizCollaborator{}).Status; s != "pending" {
		t.Fatalf("unlinked grant reported %q", s)
	}
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

// sharedQuizItem is what an editor sees about someone else's quiz. Assert the
// owner's identifying columns are not on it — model.Quiz carries host_id, and
// preloading the owner would carry their email.
func TestSharedQuizItemDoesNotLeakTheAuthorsIdentity(t *testing.T) {
	leaky := map[string]bool{"host_id": true, "email": true, "host": true, "author": true}
	rt := reflect.TypeOf(sharedQuizItem{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if leaky[name] {
			t.Fatalf("sharedQuizItem exposes %q to editors", name)
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

// An editor asking for an owner-only action gets an honest 403 that says so.
func TestDenyQuizAccessTellsAnEditorItIsOwnerOnly(t *testing.T) {
	code, msg := denyOf(t, quizRights{View: true, Edit: true})
	if code != http.StatusForbidden {
		t.Fatalf("an editor was answered %d, want 403", code)
	}
	if msg != "Only the quiz owner can do this" {
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
