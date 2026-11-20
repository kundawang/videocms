package models

// Chapter is an ordered marker on a video's timeline. Chapters belong to the
// File record (the encoded media), not to an individual Link, so every public
// link to the same upload shows the same chapter list.
type Chapter struct {
	Model
	File      File    `json:"-"`
	FileID    uint    `gorm:"index" json:"-"`
	Title     string  `gorm:"size:120;not null" json:"title"`
	StartTime float64 `gorm:"not null" json:"start_time"`
}

// ChapterInput is the owner-facing shape for one chapter. StartTime is the
// offset from the beginning of the video in seconds.
type ChapterInput struct {
	Title     string  `json:"title"`
	StartTime float64 `json:"start_time"`
}

// ChapterWrite replaces the complete chapter list of one video. The optional
// Revision is the value returned by the previous read/write; supplying a stale
// revision is rejected so two editors cannot silently overwrite each other.
type ChapterWrite struct {
	Revision uint64         `json:"revision"`
	Chapters []ChapterInput `json:"chapters"`
}

// ChapterList is the single response shape used by the owner API, the file
// detail endpoint and the public player bootstrap data.
type ChapterList struct {
	Revision uint64        `json:"revision"`
	Chapters []ChapterItem `json:"chapters"`
}

// ChapterItem is one chapter in API order (ascending start time).
type ChapterItem struct {
	ID        uint    `json:"id"`
	Title     string  `json:"title"`
	StartTime float64 `json:"start_time"`
}

// ChapterResponse wraps a rendered ChapterList with the video's duration so a
// client can validate and seek without an extra request.
type ChapterResponse struct {
	ChapterList
	Duration float64 `json:"duration"`
}

func ChapterItems(chapters []Chapter) []ChapterItem {
	items := make([]ChapterItem, 0, len(chapters))
	for _, chapter := range chapters {
		items = append(items, ChapterItem{ID: chapter.ID, Title: chapter.Title, StartTime: chapter.StartTime})
	}
	return items
}
