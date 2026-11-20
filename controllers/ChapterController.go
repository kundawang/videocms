package controllers

import (
	"ch/kirari04/videocms/helpers"
	"ch/kirari04/videocms/logic"
	"ch/kirari04/videocms/models"
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
)

// conflictMessage keeps the user-facing text stable while including the
// winning revision, so an editor can resubmit without guessing.
func conflictMessage(err error) string {
	var conflict *logic.ChapterConflictError
	if errors.As(err, &conflict) {
		return fmt.Sprintf("chapters changed since they were loaded; reload and try again (current revision %d)", conflict.Current)
	}
	return "chapters changed since they were loaded; reload and try again"
}

// chapterRequest carries the target video for both query-style reads and
// JSON-style writes. Echo binds `query` tags for GET/DELETE and JSON for PUT,
// so one struct serves every verb without a separate DTO.
type chapterRequest struct {
	LinkID uint `validate:"required,number" query:"LinkID" json:"LinkID"`
	models.ChapterWrite
}

// GetFileChapters returns the ordered chapter list of one video to a caller
// who owns it (or to an admin). Foreign and missing videos share one response
// so the endpoint cannot be used to probe for other users' links.
func (h *Handlers) GetFileChapters(c echo.Context) error {
	var request chapterRequest
	if status, err := helpers.Validate(c, &request); err != nil {
		return c.String(status, err.Error())
	}
	admin, _ := c.Get("Admin").(bool)
	response, err := h.Logic.FileChapters(request.LinkID, c.Get("UserID").(uint), admin)
	if err != nil {
		if errors.Is(err, logic.ErrChapterForbidden) {
			return c.String(http.StatusNotFound, "video doesn't exist")
		}
		return c.NoContent(http.StatusInternalServerError)
	}
	return c.JSON(http.StatusOK, response)
}

// UpdateFileChapters atomically replaces the complete chapter list. The body
// carries the revision the editor started from; a stale revision is a 409.
func (h *Handlers) UpdateFileChapters(c echo.Context) error {
	var request chapterRequest
	if status, err := helpers.Validate(c, &request); err != nil {
		return c.String(status, err.Error())
	}
	admin, _ := c.Get("Admin").(bool)
	status, response, err := h.Logic.ReplaceChapters(request.LinkID, c.Get("UserID").(uint), admin, request.ChapterWrite)
	if err != nil {
		switch {
		case errors.Is(err, logic.ErrChapterForbidden):
			return c.String(status, "video doesn't exist")
		case errors.Is(err, logic.ErrChapterValidation):
			return c.String(status, err.Error())
		case errors.Is(err, logic.ErrChapterConflict):
			return c.String(status, conflictMessage(err))
		}
		return c.NoContent(status)
	}
	return c.JSON(status, response)
}

// DeleteFileChapters clears the chapter list. Echo binds DELETE query
// parameters, so `LinkID` and a non-zero `revision` may be supplied there; a
// non-zero revision from the JSON body is equally accepted.
func (h *Handlers) DeleteFileChapters(c echo.Context) error {
	var request chapterRequest
	if status, err := helpers.Validate(c, &request); err != nil {
		return c.String(status, err.Error())
	}
	admin, _ := c.Get("Admin").(bool)
	status, err := h.Logic.DeleteFileChapters(request.LinkID, c.Get("UserID").(uint), admin, request.Revision)
	if err != nil {
		switch {
		case errors.Is(err, logic.ErrChapterForbidden):
			return c.String(status, "video doesn't exist")
		case errors.Is(err, logic.ErrChapterConflict):
			return c.String(status, conflictMessage(err))
		}
		return c.NoContent(status)
	}
	return c.NoContent(status)
}
