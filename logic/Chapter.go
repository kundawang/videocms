package logic

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"ch/kirari04/videocms/models"

	"gorm.io/gorm"
)

const (
	maxChapterTitleRunes = 120
	maxChaptersPerVideo  = 200
)

var (
	ErrChapterForbidden  = errors.New("video doesn't exist")
	ErrChapterValidation = errors.New("invalid chapter list")
	ErrChapterConflict   = errors.New("chapters changed since they were loaded")
)

// ChapterConflictError carries the revision that won the race so a client can
// reload without another round trip. It still satisfies errors.Is against
// ErrChapterConflict.
type ChapterConflictError struct {
	Current uint64
}

func (e *ChapterConflictError) Error() string {
	return fmt.Sprintf("%s; current revision is %d", ErrChapterConflict, e.Current)
}

func (e *ChapterConflictError) Unwrap() error { return ErrChapterConflict }

// ValidateChapters normalises titles and ordering, and enforces the timeline
// rules shared by the owner API and the public player:
//   - the list must not be empty and may contain at most maxChaptersPerVideo
//   - every title must be non-blank and at most 120 runes
//   - every start time must be finite, non-negative and strictly increasing
//   - start times must not pass the end of the video when a duration is known
func ValidateChapters(input []models.ChapterInput, duration float64) ([]models.ChapterInput, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("%w: at least one chapter is required", ErrChapterValidation)
	}
	if len(input) > maxChaptersPerVideo {
		return nil, fmt.Errorf("%w: at most %d chapters are allowed", ErrChapterValidation, maxChaptersPerVideo)
	}
	chapters := make([]models.ChapterInput, 0, len(input))
	for index, chapter := range input {
		title := strings.TrimSpace(chapter.Title)
		if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > maxChapterTitleRunes || hasControlRune(title) {
			return nil, fmt.Errorf("%w: chapter %d needs a title of 1 to %d printable characters", ErrChapterValidation, index+1, maxChapterTitleRunes)
		}
		if math.IsNaN(chapter.StartTime) || math.IsInf(chapter.StartTime, 0) || chapter.StartTime < 0 {
			return nil, fmt.Errorf("%w: chapter %d has an invalid start time", ErrChapterValidation, index+1)
		}
		if duration > 0 && chapter.StartTime > duration {
			return nil, fmt.Errorf("%w: chapter %d starts after the end of the video", ErrChapterValidation, index+1)
		}
		chapters = append(chapters, models.ChapterInput{Title: title, StartTime: chapter.StartTime})
	}

	// Callers may submit in any order; the stored order is always ascending.
	// Equal start times are rejected instead of silently collapsing two
	// markers onto the same frame.
	sort.SliceStable(chapters, func(i, j int) bool { return chapters[i].StartTime < chapters[j].StartTime })
	for index := 1; index < len(chapters); index++ {
		if chapters[index].StartTime == chapters[index-1].StartTime {
			return nil, fmt.Errorf("%w: chapter %d and chapter %d share the same start time", ErrChapterValidation, index, index+1)
		}
	}
	return chapters, nil
}

func hasControlRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func loadChapters(db *gorm.DB, fileID uint) ([]models.Chapter, error) {
	var chapters []models.Chapter
	if err := db.Select("id", "file_id", "title", "start_time").
		Where("file_id = ?", fileID).
		Order("start_time ASC, id ASC").
		Find(&chapters).Error; err != nil {
		return nil, err
	}
	return chapters, nil
}

// ReplaceChapters authorises the caller, checks the optimistic revision and
// replaces the full list inside one transaction. Chapters are keyed by the
// media record (FileID), which a storage migration leaves unchanged, so a
// migration in flight cannot move or duplicate the list.
func (s *Service) ReplaceChapters(linkID, userID uint, isAdmin bool, input models.ChapterWrite) (int, *models.ChapterResponse, error) {
	var link models.Link
	query := s.Deps.DB
	if !isAdmin {
		query = query.Where("user_id = ?", userID)
	}
	if res := query.Preload("File").First(&link, linkID); res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return http.StatusNotFound, nil, ErrChapterForbidden
		}
		return http.StatusInternalServerError, nil, res.Error
	}
	if input.Chapters == nil {
		return http.StatusBadRequest, nil, fmt.Errorf("%w: chapters are required", ErrChapterValidation)
	}
	chapters := []models.ChapterInput{}
	if len(input.Chapters) > 0 {
		var err error
		chapters, err = ValidateChapters(input.Chapters, link.File.Duration)
		if err != nil {
			return http.StatusBadRequest, nil, err
		}
	}

	releaseFile := s.Deps.StorageLifecycle.FileReadLock(link.FileID)
	defer releaseFile()

	var response models.ChapterResponse
	status := http.StatusInternalServerError
	err := s.Deps.DB.Transaction(func(tx *gorm.DB) error {
		// Re-read inside the transaction so ownership, duration and the
		// revision check all see the same state as the replacement.
		var current models.Link
		currentQuery := tx
		if !isAdmin {
			currentQuery = currentQuery.Where("user_id = ?", userID)
		}
		if err := currentQuery.Preload("File").First(&current, linkID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				status = http.StatusNotFound
				return ErrChapterForbidden
			}
			return err
		}
		nextRevision, err := bumpChapterRevision(tx, current.FileID, input.Revision)
		if err != nil {
			status = chapterErrorStatus(err)
			return err
		}
		response.Revision = nextRevision
		if err := replaceChapterRows(tx, current.FileID, chapters); err != nil {
			return err
		}
		rows, err := loadChapters(tx, current.FileID)
		if err != nil {
			return err
		}
		response.Duration = current.File.Duration
		response.Chapters = models.ChapterItems(rows)
		return nil
	})
	if err != nil {
		return status, nil, err
	}
	return http.StatusOK, &response, nil
}

// replaceChapterRows swaps the full list for one media in the caller's
// transaction. Chapters are hard-deleted: they are derived data with no
// independent history, and lingering soft-deleted rows would grow the table
// on every edit.
func replaceChapterRows(tx *gorm.DB, fileID uint, chapters []models.ChapterInput) error {
	if err := tx.Unscoped().Where("file_id = ?", fileID).Delete(&models.Chapter{}).Error; err != nil {
		return err
	}
	if len(chapters) == 0 {
		return nil
	}
	rows := make([]models.Chapter, 0, len(chapters))
	for _, chapter := range chapters {
		rows = append(rows, models.Chapter{FileID: fileID, Title: chapter.Title, StartTime: chapter.StartTime})
	}
	return tx.Create(&rows).Error
}

func chapterErrorStatus(err error) int {
	if errors.Is(err, ErrChapterConflict) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// bumpChapterRevision compares and swaps the chapter counter in one UPDATE.
// Two writers that both submit the same revision cannot both win, even across
// separate database connections, because only the matching row is changed.
func bumpChapterRevision(tx *gorm.DB, fileID uint, expected uint64) (uint64, error) {
	result := tx.Model(&models.File{}).
		Where("id = ? AND chapters_revision = ?", fileID, expected).
		UpdateColumn("chapters_revision", gorm.Expr("chapters_revision + 1"))
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected == 1 {
		return expected + 1, nil
	}
	var current models.File
	if err := tx.Select("id", "chapters_revision").First(&current, fileID).Error; err != nil {
		return 0, err
	}
	return current.ChaptersRevision, &ChapterConflictError{Current: current.ChaptersRevision}
}

// FileChapters returns the current chapter list of a link the caller can see.
// It is used by the owner file detail endpoint.
func (s *Service) FileChapters(linkID, userID uint, isAdmin bool) (*models.ChapterResponse, error) {
	query := s.Deps.DB
	if !isAdmin {
		query = query.Where("user_id = ?", userID)
	}
	var link models.Link
	if err := query.Preload("File").First(&link, linkID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrChapterForbidden
		}
		return nil, err
	}
	chapters, err := loadChapters(s.Deps.DB, link.FileID)
	if err != nil {
		return nil, err
	}
	return &models.ChapterResponse{
		ChapterList: models.ChapterList{Revision: link.File.ChaptersRevision, Chapters: models.ChapterItems(chapters)},
		Duration:    link.File.Duration,
	}, nil
}

// DeleteFileChapters removes every chapter marker of one video. The owner
// scoping mirrors ReplaceChapters; a stale revision still rejects the write.
func (s *Service) DeleteFileChapters(linkID, userID uint, isAdmin bool, revision uint64) (int, error) {
	var link models.Link
	query := s.Deps.DB
	if !isAdmin {
		query = query.Where("user_id = ?", userID)
	}
	if err := query.Preload("File").First(&link, linkID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return http.StatusNotFound, ErrChapterForbidden
		}
		return http.StatusInternalServerError, err
	}
	releaseFile := s.Deps.StorageLifecycle.FileReadLock(link.FileID)
	defer releaseFile()

	status := http.StatusInternalServerError
	err := s.Deps.DB.Transaction(func(tx *gorm.DB) error {
		var current models.Link
		currentQuery := tx
		if !isAdmin {
			currentQuery = currentQuery.Where("user_id = ?", userID)
		}
		if err := currentQuery.First(&current, linkID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				status = http.StatusNotFound
				return ErrChapterForbidden
			}
			return err
		}
		if _, err := bumpChapterRevision(tx, current.FileID, revision); err != nil {
			status = chapterErrorStatus(err)
			return err
		}
		if err := tx.Unscoped().Where("file_id = ?", current.FileID).Delete(&models.Chapter{}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return status, err
	}
	return http.StatusNoContent, nil
}
