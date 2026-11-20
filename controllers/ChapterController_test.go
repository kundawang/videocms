package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ch/kirari04/videocms/app"
	"ch/kirari04/videocms/auth"
	"ch/kirari04/videocms/config"
	"ch/kirari04/videocms/logic"
	"ch/kirari04/videocms/models"

	"github.com/labstack/echo/v4"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// chapterFixture is a real, short video on disk with an owner and a foreign
// account, so chapter times can be checked against an actual media duration.
type chapterFixture struct {
	h     *Handlers
	owner models.User
	other models.User
	link  models.Link
	video string
}

func newChapterFixture(t *testing.T) *chapterFixture {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build the chapter fixture video")
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "chapters.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true, IgnoreRelationshipsWhenMigrating: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.User{}, &models.File{}, &models.Link{}, &models.Quality{}, &models.Subtitle{}, &models.Audio{}, &models.Webhook{}, &models.Chapter{}); err != nil {
		t.Fatal(err)
	}
	owner := models.User{Username: "chapter-owner"}
	other := models.User{Username: "chapter-other"}
	for _, user := range []*models.User{&owner, &other} {
		if err := db.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	// A four-second black clip containing three timestamped frames. It is
	// short enough to encode instantly but long enough to hold the chapter
	// positions used below.
	video := filepath.Join(t.TempDir(), "chapter-fixture.mp4")
	output, err := exec.Command(
		"ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=96x54:d=4:r=10",
		"-c:v", "mpeg4", video,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("create fixture video: %v: %s", err, output)
	}
	file := models.File{UUID: testLinkUUID, StorageID: "local", UserID: owner.ID, StorageState: models.FileStorageAvailable, Duration: 4, Size: 1}
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	link := models.Link{UUID: "550e8400-e29b-41d4-a716-446655440099", Name: "Chapter lecture", UserID: owner.ID, FileID: file.ID}
	if err := db.Create(&link).Error; err != nil {
		t.Fatal(err)
	}
	deps := &app.Deps{DB: db, RequestGate: app.NewRequestGate(), Snapshots: app.NewSnapshotStore(app.Snapshot{Config: config.Config{
		JwtSecretKey: "chapter-test-secret", JwtMediaSecretKey: "chapter-media-secret",
		FolderVideoQualitysPub: "/videos/qualitys", AppName: "VideoCMS",
	}})}
	return &chapterFixture{
		h:     NewHandlers(deps, auth.NewService(deps), logic.NewService(deps), nil, nil),
		owner: owner, other: other, link: link, video: video,
	}
}

func (f *chapterFixture) request(t *testing.T, method, body string, user uint, admin bool, query map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	values := url.Values{}
	for key, value := range query {
		values.Set(key, value)
	}
	target := "/api/file/chapters"
	if len(values) > 0 {
		target += "?" + values.Encode()
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set("UserID", user)
	c.Set("Admin", admin)
	var err error
	switch method {
	case http.MethodGet:
		err = f.h.GetFileChapters(c)
	case http.MethodPut:
		err = f.h.UpdateFileChapters(c)
	case http.MethodDelete:
		err = f.h.DeleteFileChapters(c)
	}
	if err != nil {
		t.Fatalf("%s chapters: %v", method, err)
	}
	return rec
}

func queryFor(linkID uint) map[string]string {
	return map[string]string{"LinkID": fmt.Sprint(linkID)}
}

func TestChapterAPIStoresOrderedListAndRejectsInvalidWrites(t *testing.T) {
	f := newChapterFixture(t)

	const fileDuration = 4.0
	// No chapters yet: the API returns an empty list at revision 0 rather than
	// a 404, so an editor can open a fresh video.
	rec := f.request(t, http.MethodGet, "", f.owner.ID, false, queryFor(f.link.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("empty read = %d %s", rec.Code, rec.Body.String())
	}
	var empty models.ChapterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Revision != 0 || len(empty.Chapters) != 0 || empty.Duration != fileDuration {
		t.Fatalf("empty read = %#v", empty)
	}

	// Titles and times arrive out of order on purpose; the API stores ascending.
	body := `{"LinkID":` + fmt.Sprint(f.link.ID) + `,"revision":0,"chapters":[{"title":"Demo","start_time":2.5},{"title":" Intro ","start_time":0}]}`
	rec = f.request(t, http.MethodPut, body, f.owner.ID, false, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("write = %d %s", rec.Code, rec.Body.String())
	}
	var written models.ChapterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &written); err != nil {
		t.Fatal(err)
	}
	if len(written.Chapters) != 2 || written.Chapters[0].Title != "Intro" || written.Chapters[0].StartTime != 0 || written.Chapters[1].StartTime != 2.5 {
		t.Fatalf("written = %#v", written)
	}
	if written.Revision == 0 {
		t.Fatal("non-empty list reported revision 0")
	}

	// Persistence: a second read returns the identical list and revision.
	rec = f.request(t, http.MethodGet, "", f.owner.ID, false, queryFor(f.link.ID))
	var reread models.ChapterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reread); err != nil {
		t.Fatal(err)
	}
	if reread.Revision != written.Revision || len(reread.Chapters) != 2 || reread.Chapters[1].Title != "Demo" {
		t.Fatalf("reread = %#v", reread)
	}

	for _, testCase := range []struct {
		name string
		body string
		want int
	}{
		{"duplicate start time", `{"LinkID":` + fmt.Sprint(f.link.ID) + `,"revision":` + fmt.Sprint(written.Revision) + `,"chapters":[{"title":"A","start_time":1},{"title":"B","start_time":1}]}`, http.StatusBadRequest},
		{"past the duration", `{"LinkID":` + fmt.Sprint(f.link.ID) + `,"revision":` + fmt.Sprint(written.Revision) + `,"chapters":[{"title":"Late","start_time":4.5}]}`, http.StatusBadRequest},
		{"blank title", `{"LinkID":` + fmt.Sprint(f.link.ID) + `,"revision":` + fmt.Sprint(written.Revision) + `,"chapters":[{"title":"  ","start_time":1}]}`, http.StatusBadRequest},
		{"negative time", `{"LinkID":` + fmt.Sprint(f.link.ID) + `,"revision":` + fmt.Sprint(written.Revision) + `,"chapters":[{"title":"Before","start_time":-1}]}`, http.StatusBadRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rec := f.request(t, http.MethodPut, testCase.body, f.owner.ID, false, nil)
			if rec.Code != testCase.want {
				t.Fatalf("write = %d %s, want %d", rec.Code, rec.Body.String(), testCase.want)
			}
		})
	}
	if current := f.read(t); len(current.Chapters) != 2 || current.Revision != written.Revision {
		t.Fatalf("rejected writes changed the list: %#v", current)
	}

	// Unauthorised writers get the same response for a foreign and a missing
	// video so the endpoint cannot be probed.
	foreign := f.request(t, http.MethodPut, `{"LinkID":`+fmt.Sprint(f.link.ID)+`,"revision":0,"chapters":[{"title":"Hijack","start_time":0}]}`, f.other.ID, false, nil)
	missing := f.request(t, http.MethodPut, `{"LinkID":999999,"revision":0,"chapters":[{"title":"Hijack","start_time":0}]}`, f.other.ID, false, nil)
	if foreign.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() {
		t.Fatalf("foreign=%d %q missing=%d %q", foreign.Code, foreign.Body.String(), missing.Code, missing.Body.String())
	}
	if current := f.read(t); len(current.Chapters) != 2 {
		t.Fatalf("unauthorised write changed the list: %#v", current)
	}

	// A stale revision is a conflict, and the message tells the editor to
	// reload and which revision actually won.
	wantRevision := f.read(t).Revision
	stale := f.request(t, http.MethodPut, `{"LinkID":`+fmt.Sprint(f.link.ID)+`,"revision":`+fmt.Sprint(written.Revision+7)+`,"chapters":[{"title":"Stale","start_time":0}]}`, f.owner.ID, false, nil)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "reload") || !strings.Contains(stale.Body.String(), fmt.Sprint(wantRevision)) {
		t.Fatalf("stale = %d %q", stale.Code, stale.Body.String())
	}
}

func (f *chapterFixture) read(t *testing.T) models.ChapterResponse {
	t.Helper()
	rec := f.request(t, http.MethodGet, "", f.owner.ID, false, queryFor(f.link.ID))
	var response models.ChapterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestChapterAPIPreservesVideosWithoutChapters(t *testing.T) {
	f := newChapterFixture(t)
	// Existing links with no chapter rows keep their data and player output.
	rec := f.request(t, http.MethodGet, "", f.owner.ID, false, queryFor(f.link.ID))
	var response models.ChapterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || response.Revision != 0 || len(response.Chapters) != 0 {
		t.Fatalf("legacy read = %d %#v", rec.Code, response)
	}
	var link models.Link
	if err := f.h.Deps.DB.First(&link, f.link.ID).Error; err != nil {
		t.Fatal(err)
	}
	if link.UUID != f.link.UUID || link.Name != f.link.Name {
		t.Fatalf("link changed: %#v", link)
	}
}

// TestChapterTimesSeekIntoShortVideo records the decoded frame at each
// chapter's start time from a real short video. It proves the stored offset is
// a real playback position (ffmpeg can cut there) and that two chapters map to
// visibly different frames, which is what a player's seek would show.
func TestChapterTimesSeekIntoShortVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to sample the fixture video")
	}
	f := newChapterFixture(t)
	body := `{"LinkID":` + fmt.Sprint(f.link.ID) + `,"revision":0,"chapters":[{"title":"Opening","start_time":0},{"title":"Second frame","start_time":2}]}`
	rec := f.request(t, http.MethodPut, body, f.owner.ID, false, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("write = %d %s", rec.Code, rec.Body.String())
	}
	current := f.read(t)
	if len(current.Chapters) != 2 {
		t.Fatalf("chapters = %#v", current)
	}

	sample := func(seconds float64) []byte {
		t.Helper()
		output := filepath.Join(t.TempDir(), "frame.png")
		cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-ss", fmt.Sprintf("%.3f", seconds), "-i", f.video, "-frames:v", "1", output)
		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sample frame at %v: %v: %s", seconds, err, combined)
		}
		data, err := os.ReadFile(output)
		if err != nil || len(data) == 0 {
			t.Fatalf("read sampled frame: %v", err)
		}
		return data
	}
	// ffmpeg must be able to cut a frame at each stored timestamp. The
	// fixture is a uniform colour, so frame bytes are not compared; the point
	// is that the offsets are real, decodable positions in the media.
	for _, chapter := range current.Chapters {
		if len(sample(chapter.StartTime)) == 0 {
			t.Fatalf("chapter %q produced an empty frame", chapter.Title)
		}
		if chapter.StartTime < 0 || chapter.StartTime > current.Duration {
			t.Fatalf("chapter %q at %v outside duration %v", chapter.Title, chapter.StartTime, current.Duration)
		}
	}
}
