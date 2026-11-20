package controllers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ch/kirari04/videocms/models"

	"github.com/labstack/echo/v4"
)

type chapterPlayerRenderer struct {
	lastName string
	lastData map[string]any
}

func (r *chapterPlayerRenderer) Render(w io.Writer, name string, data any, c echo.Context) error {
	r.lastName = name
	if values, ok := data.(echo.Map); ok {
		r.lastData = values
	}
	_, err := io.WriteString(w, name)
	return err
}

// TestPlayerControllerBootstrapsChaptersForPublicLinks pins the contract the
// player relies on: the public link player publishes the same ordered list and
// revision that the owner API returns, and a video without chapters publishes
// an empty list rather than failing.
func TestPlayerControllerBootstrapsChaptersForPublicLinks(t *testing.T) {
	f := newChapterFixture(t)
	renderer := &chapterPlayerRenderer{}

	run := func() map[string]any {
		t.Helper()
		e := echo.New()
		e.Renderer = renderer
		req := httptest.NewRequest(http.MethodGet, "/v/"+f.link.UUID, nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetParamNames("UUID")
		c.SetParamValues(f.link.UUID)
		if err := f.h.PlayerController(c); err != nil {
			t.Fatalf("PlayerController() error = %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("PlayerController() status = %d", rec.Code)
		}
		for _, key := range []string{"Chapters", "UUID", "StreamIsReady", "Folder"} {
			if _, ok := renderer.lastData[key]; !ok {
				t.Fatalf("player data missing %q", key)
			}
		}
		if renderer.lastData["UUID"] != f.link.UUID {
			t.Fatalf("player UUID = %v", renderer.lastData["UUID"])
		}
		var chapters models.ChapterList
		if err := json.Unmarshal([]byte(renderer.lastData["Chapters"].(string)), &chapters); err != nil {
			t.Fatalf("player chapter payload: %v", err)
		}
		raw := map[string]any{}
		if err := json.Unmarshal([]byte(renderer.lastData["Chapters"].(string)), &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}

	plain := run()
	if revision, _ := plain["revision"].(float64); revision != 0 {
		t.Fatalf("no-chapter revision = %v", plain["revision"])
	}
	if chapters, _ := plain["chapters"].([]any); len(chapters) != 0 {
		t.Fatalf("no-chapter list = %v", plain["chapters"])
	}

	body := `{"LinkID":` + fmt.Sprint(f.link.ID) + `,"revision":0,"chapters":[{"title":"Intro","start_time":0},{"title":"Demo","start_time":2}]}`
	if rec := f.request(t, http.MethodPut, body, f.owner.ID, false, nil); rec.Code != http.StatusOK {
		t.Fatalf("write = %d %s", rec.Code, rec.Body.String())
	}
	api := f.read(t)

	withChapters := run()
	// The revision is intentionally small enough to round-trip through JSON
	// in JavaScript; the player and API must report the same number exactly.
	if revision, _ := withChapters["revision"].(float64); uint64(revision) != api.Revision {
		t.Fatalf("player revision = %v, API revision = %d", withChapters["revision"], api.Revision)
	}
	raw, _ := json.Marshal(withChapters["chapters"])
	var published []models.ChapterItem
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatal(err)
	}
	if len(published) != len(api.Chapters) {
		t.Fatalf("player chapters = %d, API chapters = %d", len(published), len(api.Chapters))
	}
	for index := range published {
		if published[index] != api.Chapters[index] {
			t.Fatalf("chapter %d differs: player=%#v api=%#v", index, published[index], api.Chapters[index])
		}
	}

	// The payload the controller produced must survive template rendering in
	// a script-readable form. Retrieval is checked end-to-end by the goja
	// player tests, which execute the rendered script.
	if !strings.Contains(renderer.lastData["Chapters"].(string), `"Demo"`) {
		t.Fatalf("player chapter payload lost the stored chapter")
	}
}

// TestSharedVideoPlayerBootstrapsTheSameChapters checks the folder-share
// wrapper. A visitor reaching the player through /share-videos/:token/:uuid
// must see the list the owner saved, because the wrapper calls the same
// PlayerController.
func TestSharedVideoPlayerBootstrapsTheSameChapters(t *testing.T) {
	f := newChapterFixture(t)
	renderer := &chapterPlayerRenderer{}
	if err := f.h.Deps.DB.AutoMigrate(&models.Folder{}, &models.FolderShare{}); err != nil {
		t.Fatal(err)
	}
	folder := models.Folder{Name: "Shared", UserID: f.owner.ID}
	if err := f.h.Deps.DB.Create(&folder).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.h.Deps.DB.Model(&models.Link{}).Where("id = ?", f.link.ID).Update("parent_folder_id", folder.ID).Error; err != nil {
		t.Fatal(err)
	}
	share := models.FolderShare{Token: strings.Repeat("a", 43), UserID: f.owner.ID, FolderID: folder.ID, AllowRead: true, AccessVersion: "v1"}
	if err := f.h.Deps.DB.Create(&share).Error; err != nil {
		t.Fatal(err)
	}
	body := `{"LinkID":` + fmt.Sprint(f.link.ID) + `,"revision":0,"chapters":[{"title":"Intro","start_time":0}]}`
	if rec := f.request(t, http.MethodPut, body, f.owner.ID, false, nil); rec.Code != http.StatusOK {
		t.Fatalf("write = %d %s", rec.Code, rec.Body.String())
	}
	api := f.read(t)

	wrapped := f.h.SharedVideo(f.h.PlayerController)
	e := echo.New()
	e.Renderer = renderer
	req := httptest.NewRequest(http.MethodGet, "/share-videos/"+share.Token+"/"+f.link.UUID, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token", "UUID")
	c.SetParamValues(share.Token, f.link.UUID)
	if err := wrapped(c); err != nil {
		t.Fatalf("SharedVideo() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("SharedVideo() status = %d", rec.Code)
	}
	var published models.ChapterList
	if err := json.Unmarshal([]byte(renderer.lastData["Chapters"].(string)), &published); err != nil {
		t.Fatal(err)
	}
	if published.Revision != api.Revision || len(published.Chapters) != 1 || published.Chapters[0].Title != "Intro" {
		t.Fatalf("shared player chapters = %#v, API = %#v", published, api)
	}
}
