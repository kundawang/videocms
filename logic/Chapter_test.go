package logic

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"

	"ch/kirari04/videocms/models"
)

func TestValidateChaptersOrdersAndNormalisesInput(t *testing.T) {
	duration := 600.0
	chapters, err := ValidateChapters([]models.ChapterInput{
		{Title: "  Second part  ", StartTime: 120},
		{Title: "Intro", StartTime: 0},
		{Title: "Middle", StartTime: 60.5},
	}, duration)
	if err != nil {
		t.Fatalf("ValidateChapters() error = %v", err)
	}
	wantTitles := []string{"Intro", "Middle", "Second part"}
	wantTimes := []float64{0, 60.5, 120}
	if len(chapters) != len(wantTitles) {
		t.Fatalf("chapters = %#v", chapters)
	}
	for index := range chapters {
		if chapters[index].Title != wantTitles[index] || chapters[index].StartTime != wantTimes[index] {
			t.Fatalf("chapter %d = %#v, want %q@%v", index, chapters[index], wantTitles[index], wantTimes[index])
		}
	}
}

func TestValidateChaptersRejectsInvalidInput(t *testing.T) {
	duration := 90.0
	for _, testCase := range []struct {
		name     string
		chapters []models.ChapterInput
	}{
		{"empty list", nil},
		{"blank title", []models.ChapterInput{{Title: "   ", StartTime: 0}}},
		{"control character in title", []models.ChapterInput{{Title: "Intro\nEnd", StartTime: 0}}},
		{"title over 120 runes", []models.ChapterInput{{Title: string(make([]rune, maxChapterTitleRunes+1)), StartTime: 0}}},
		{"negative time", []models.ChapterInput{{Title: "Intro", StartTime: -1}}},
		{"NaN time", []models.ChapterInput{{Title: "Intro", StartTime: math.NaN()}}},
		{"infinite time", []models.ChapterInput{{Title: "Intro", StartTime: math.Inf(1)}}},
		{"past duration", []models.ChapterInput{{Title: "Intro", StartTime: duration + 0.1}}},
		{"duplicate times", []models.ChapterInput{{Title: "A", StartTime: 5}, {Title: "B", StartTime: 5}}},
		{"too many chapters", tooManyChapters()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ValidateChapters(testCase.chapters, duration); !errors.Is(err, ErrChapterValidation) {
				t.Fatalf("ValidateChapters() error = %v, want ErrChapterValidation", err)
			}
		})
	}
}

func tooManyChapters() []models.ChapterInput {
	chapters := make([]models.ChapterInput, 0, maxChaptersPerVideo+1)
	for index := 0; index <= maxChaptersPerVideo; index++ {
		chapters = append(chapters, models.ChapterInput{Title: fmt.Sprintf("Chapter %d", index), StartTime: float64(index)})
	}
	return chapters
}

func TestValidateChaptersAllowsExactDurationAndZeroDurationVideo(t *testing.T) {
	if _, err := ValidateChapters([]models.ChapterInput{{Title: "End", StartTime: 90}}, 90); err != nil {
		t.Fatalf("chapter at exactly the duration: %v", err)
	}
	// A not-yet-probed video (duration 0) cannot bound the timeline; basic
	// ordering and title rules still apply.
	if _, err := ValidateChapters([]models.ChapterInput{{Title: "Intro", StartTime: 500}}, 0); err != nil {
		t.Fatalf("unknown duration: %v", err)
	}
}

func TestChapterRevisionIsAPerMediaCounter(t *testing.T) {
	s, owner, link := chapterTestService(t)
	// No chapters, nothing written: revision 0.
	empty, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil || empty.Revision != 0 {
		t.Fatalf("initial revision = %#v err=%v", empty, err)
	}
	status, first, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: 0,
		Chapters: []models.ChapterInput{{Title: "Intro", StartTime: 0}},
	})
	if status != http.StatusOK || err != nil {
		t.Fatalf("first write = (%d, %v)", status, err)
	}
	if first.Revision != 1 {
		t.Fatalf("first revision = %d, want 1", first.Revision)
	}
	// Editing the same content still advances the counter, so a second editor
	// holding the old value cannot overwrite the first edit unnoticed.
	status, second, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: first.Revision,
		Chapters: []models.ChapterInput{{Title: "Intro", StartTime: 0}},
	})
	if status != http.StatusOK || err != nil || second.Revision != 2 {
		t.Fatalf("second write = (%d, %#v, %v)", status, second, err)
	}
	// Clearing the list does not reset the counter; the revision only moves
	// forward, so a stale editor cannot replay a cleared list.
	if status, err := s.DeleteFileChapters(link.ID, owner.ID, false, second.Revision); status != http.StatusNoContent || err != nil {
		t.Fatalf("delete = (%d, %v)", status, err)
	}
	cleared, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil || len(cleared.Chapters) != 0 || cleared.Revision != 3 {
		t.Fatalf("cleared = %#v err=%v", cleared, err)
	}
	if status, _, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: 0, Chapters: []models.ChapterInput{{Title: "Replay", StartTime: 0}},
	}); status != http.StatusConflict || !errors.Is(err, ErrChapterConflict) {
		t.Fatalf("replayed revision 0 = (%d, %v)", status, err)
	}
}

func chapterTestService(t *testing.T) (*Service, models.User, models.Link) {
	t.Helper()
	s, owner, _ := ownershipTestService(t)
	if err := s.Deps.DB.AutoMigrate(&models.Chapter{}); err != nil {
		t.Fatal(err)
	}
	link := ownershipLink(t, s, owner.ID, 0)
	if err := s.Deps.DB.Model(&models.File{}).Where("id = ?", link.FileID).
		Update("duration", 300).Error; err != nil {
		t.Fatal(err)
	}
	return s, owner, link
}

func TestReplaceChaptersPersistsAndAuthorises(t *testing.T) {
	s, owner, link := chapterTestService(t)
	foreign := models.User{Username: "chapter-foreign"}
	if err := s.Deps.DB.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}

	status, response, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: 0,
		Chapters: []models.ChapterInput{{Title: "Intro", StartTime: 0}, {Title: "Demo", StartTime: 42}},
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("ReplaceChapters() = (%d, %v)", status, err)
	}
	if response.Duration != 300 || len(response.Chapters) != 2 || response.Revision == 0 {
		t.Fatalf("response = %#v", response)
	}
	if response.Chapters[1].Title != "Demo" || response.Chapters[1].StartTime != 42 {
		t.Fatalf("stored chapter = %#v", response.Chapters[1])
	}

	// A second user cannot read or write another account's chapters, and the
	// response never reveals whether the link exists.
	if _, err := s.FileChapters(link.ID, foreign.ID, false); !errors.Is(err, ErrChapterForbidden) {
		t.Fatalf("foreign read error = %v", err)
	}
	status, _, err = s.ReplaceChapters(link.ID, foreign.ID, false, models.ChapterWrite{
		Revision: response.Revision,
		Chapters: []models.ChapterInput{{Title: "Hijack", StartTime: 0}},
	})
	if status != http.StatusNotFound || !errors.Is(err, ErrChapterForbidden) {
		t.Fatalf("foreign write = (%d, %v)", status, err)
	}
	status, _, err = s.ReplaceChapters(999999, foreign.ID, false, models.ChapterWrite{
		Revision: 0, Chapters: []models.ChapterInput{{Title: "Hijack", StartTime: 0}},
	})
	if status != http.StatusNotFound || !errors.Is(err, ErrChapterForbidden) {
		t.Fatalf("missing write = (%d, %v)", status, err)
	}
	stored, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil || len(stored.Chapters) != 2 || stored.Chapters[0].Title != "Intro" {
		t.Fatalf("foreign write changed chapters: %#v err=%v", stored, err)
	}

	// An admin may edit any account's chapters.
	if status, _, err := s.ReplaceChapters(link.ID, foreign.ID, true, models.ChapterWrite{
		Revision: stored.Revision,
		Chapters: []models.ChapterInput{{Title: "Admin edit", StartTime: 0}},
	}); status != http.StatusOK || err != nil {
		t.Fatalf("admin write = (%d, %v)", status, err)
	}
}

func TestReplaceChaptersRejectsStaleRevisionAndKeepsCurrentList(t *testing.T) {
	s, owner, link := chapterTestService(t)
	status, first, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: 0,
		Chapters: []models.ChapterInput{{Title: "Original", StartTime: 0}},
	})
	if status != http.StatusOK || err != nil {
		t.Fatalf("initial write = (%d, %v)", status, err)
	}

	// Two editors both start from the same revision. The first wins; the
	// second gets a conflict and the winner's list is untouched.
	winner := models.ChapterWrite{Revision: first.Revision, Chapters: []models.ChapterInput{{Title: "Winner", StartTime: 0}}}
	loser := models.ChapterWrite{Revision: first.Revision, Chapters: []models.ChapterInput{{Title: "Loser", StartTime: 0}}}
	if status, _, err := s.ReplaceChapters(link.ID, owner.ID, false, winner); status != http.StatusOK || err != nil {
		t.Fatalf("winner = (%d, %v)", status, err)
	}
	status, response, err := s.ReplaceChapters(link.ID, owner.ID, false, loser)
	if status != http.StatusConflict || !errors.Is(err, ErrChapterConflict) || response != nil {
		t.Fatalf("loser = (%d, %#v, %v)", status, response, err)
	}
	current, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil || len(current.Chapters) != 1 || current.Chapters[0].Title != "Winner" {
		t.Fatalf("current chapters = %#v err=%v", current, err)
	}

	// The conflict is recoverable: resubmitting with the fresh revision works.
	if status, _, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: current.Revision,
		Chapters: []models.ChapterInput{{Title: "Winner", StartTime: 0}, {Title: "Loser", StartTime: 30}},
	}); status != http.StatusOK || err != nil {
		t.Fatalf("retry after conflict = (%d, %v)", status, err)
	}
}

func TestReplaceChaptersRequiresExplicitListAndDeletesAtRevision(t *testing.T) {
	s, owner, link := chapterTestService(t)
	status, _, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{Revision: 0})
	if status != http.StatusBadRequest || !errors.Is(err, ErrChapterValidation) {
		t.Fatalf("missing list = (%d, %v)", status, err)
	}

	// An explicitly empty list is accepted and clears the video. It still
	// bumps the revision so a stale editor cannot replay an old list.
	emptyWrite, _, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{Revision: 0, Chapters: []models.ChapterInput{}})
	if emptyWrite != http.StatusOK || err != nil {
		t.Fatalf("empty list = (%d, %v)", emptyWrite, err)
	}
	afterEmpty, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil || len(afterEmpty.Chapters) != 0 || afterEmpty.Revision != 1 {
		t.Fatalf("after empty list = %#v err=%v", afterEmpty, err)
	}

	status, response, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: afterEmpty.Revision, Chapters: []models.ChapterInput{{Title: "Keep me", StartTime: 0}},
	})
	if status != http.StatusOK || err != nil {
		t.Fatalf("seed = (%d, %v)", status, err)
	}
	// A delete with an old revision is rejected even though the intent is
	// destructive; the caller must have seen the current list first.
	if status, err := s.DeleteFileChapters(link.ID, owner.ID, false, response.Revision+1); status != http.StatusConflict || !errors.Is(err, ErrChapterConflict) {
		t.Fatalf("stale delete = (%d, %v)", status, err)
	}
	if status, err := s.DeleteFileChapters(link.ID, owner.ID, false, response.Revision); status != http.StatusNoContent || err != nil {
		t.Fatalf("delete = (%d, %v)", status, err)
	}
	remaining, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil || len(remaining.Chapters) != 0 || remaining.Revision != response.Revision+1 {
		t.Fatalf("chapters after delete = %#v err=%v", remaining, err)
	}
}

func TestReplaceChaptersRejectsDurationOverflow(t *testing.T) {
	s, owner, link := chapterTestService(t)
	status, _, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: 0,
		Chapters: []models.ChapterInput{{Title: "Past the end", StartTime: 301}},
	})
	if status != http.StatusBadRequest || !errors.Is(err, ErrChapterValidation) {
		t.Fatalf("overflow write = (%d, %v)", status, err)
	}
	chapters, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil || len(chapters.Chapters) != 0 {
		t.Fatalf("rejected write persisted chapters: %#v err=%v", chapters, err)
	}
}

func TestFileDetailIncludesChapters(t *testing.T) {
	s, owner, link := chapterTestService(t)
	if status, _, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: 0,
		Chapters: []models.ChapterInput{{Title: "Intro", StartTime: 0}},
	}); status != http.StatusOK || err != nil {
		t.Fatalf("seed = (%d, %v)", status, err)
	}
	status, detail, err := s.GetFile(link.ID, owner.ID, false)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GetFile() = (%d, %v)", status, err)
	}
	if len(detail.Chapters.Chapters) != 1 || detail.Chapters.Chapters[0].Title != "Intro" || detail.Chapters.Revision == 0 {
		t.Fatalf("detail chapters = %#v", detail.Chapters)
	}
}

func TestReplaceChaptersSerialisesConcurrentWriters(t *testing.T) {
	s, owner, link := chapterTestService(t)
	// The fixture keeps SQLite on a single connection, so writers in separate
	// goroutines contend for the same database. The revision check must still
	// admit exactly one of them.
	const writers = 12
	start := make(chan struct{})
	results := make(chan int, writers)
	for index := 0; index < writers; index++ {
		go func(index int) {
			<-start
			// Every writer claims revision 0, exactly as two browser tabs that
			// loaded the editor together would.
			status, _, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
				Revision: 0,
				Chapters: []models.ChapterInput{{Title: fmt.Sprintf("Writer %d", index), StartTime: 0}},
			})
			if err != nil && !errors.Is(err, ErrChapterConflict) {
				t.Errorf("concurrent writer %d: %v", index, err)
				status = -1
			}
			results <- status
		}(index)
	}
	close(start)
	ok := 0
	for index := 0; index < writers; index++ {
		switch status := <-results; status {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected concurrent result %d", status)
		}
	}
	// Revision 0 means "never written". Only one writer can advance the
	// counter; every other writer gets a conflict instead of overwriting it.
	if ok != 1 {
		t.Fatalf("%d concurrent writers committed, want exactly 1", ok)
	}
	current, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil || len(current.Chapters) != 1 || current.Revision != 1 {
		t.Fatalf("concurrent writes left %#v err=%v", current, err)
	}
}

// TestChaptersSurviveStorageMigration pins the migration contract: chapters
// hang off the File row, which a storage cutover updates in place, so moving
// media between pools neither moves nor duplicates the list.
func TestChaptersSurviveStorageMigration(t *testing.T) {
	s, owner, link := chapterTestService(t)
	if status, _, err := s.ReplaceChapters(link.ID, owner.ID, false, models.ChapterWrite{
		Revision: 0,
		Chapters: []models.ChapterInput{{Title: "Intro", StartTime: 0}, {Title: "Middle", StartTime: 30}},
	}); status != http.StatusOK || err != nil {
		t.Fatalf("seed = (%d, %v)", status, err)
	}
	before, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil {
		t.Fatal(err)
	}

	// The cutover only updates storage columns on files. Mirror that write for
	// the link's media and confirm the chapter list is untouched.
	if err := s.Deps.DB.Model(&models.File{}).Where("id = ?", link.FileID).Updates(map[string]any{
		"storage_id": "destination", "storage_pool_id": 7,
	}).Error; err != nil {
		t.Fatal(err)
	}
	afterMove, err := s.FileChapters(link.ID, owner.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if afterMove.Revision != before.Revision || len(afterMove.Chapters) != 2 || afterMove.Chapters[1].Title != "Middle" {
		t.Fatalf("chapters changed by migration: before=%#v after=%#v", before, afterMove)
	}

	// Many links can point at the same media; each must expose the same list.
	second := models.Link{UUID: "550e8400-e29b-41d4-a716-446655440077", Name: "same upload", UserID: owner.ID, FileID: link.FileID}
	if err := s.Deps.DB.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	shared, err := s.FileChapters(second.ID, owner.ID, false)
	if err != nil || shared.Revision != before.Revision || len(shared.Chapters) != 2 {
		t.Fatalf("shared media chapters = %#v err=%v", shared, err)
	}
}
