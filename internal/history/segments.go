package history

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Turn is one stored row of a transcript as the API reports it.
type Turn struct {
	TurnID string
	// N is the turn number inside the session; the user's row and the
	// assistant's row of one turn share it.
	N int64
	// Role is "user" or "assistant".
	Role string
	// Act is the directive act of an assistant row.
	Act  string
	Body string
	// Clipped is true when Body was cut when it was stored.
	Clipped   bool
	CreatedAt time.Time
	// Withheld is true when Body is empty because the text of an assistant row
	// was refused by the sanitiser that every response passes on its way to a
	// client.
	Withheld bool
}

// SegmentPage is one page of a session's transcript.
type SegmentPage struct {
	SessionID string
	// Turns are the rows of up to the requested number of whole turns, oldest
	// first.
	Turns []Turn
	// NextCursor is the turn number to continue after, or 0 when this page
	// reaches the end of the transcript.
	NextCursor int64
	// Total is how many rows the session's transcript holds now.
	Total int64
}

// Segments returns a page of the transcript of one of the caller's sessions: up
// to limit whole turns (DefaultPage when limit is zero, at most MaxPage) after
// the turn number cursor (0 starts at the beginning). The session is named by
// id and need not be the session of the token, but it must be the caller's own:
// another principal's session is scope_violation. A session without a
// transcript is an empty page.
//
// A page never splits a turn, so continuing from NextCursor can neither skip nor
// repeat a row.
func (s *Service) Segments(ctx context.Context, scope contracts.Scope, sessionID string, cursor int64, limit int) (SegmentPage, error) {
	if err := s.authorize(ctx, scope, ActionRead, CapRead); err != nil {
		return SegmentPage{}, err
	}
	if !validID(sessionID) {
		return SegmentPage{}, badSessionID()
	}
	if cursor < 0 {
		return SegmentPage{}, contracts.New(contracts.ErrValidationFailed, "history: the cursor is negative")
	}
	switch {
	case limit == 0:
		limit = DefaultPage
	case limit < 0 || limit > MaxPage:
		return SegmentPage{}, contracts.Newf(contracts.ErrValidationFailed, "history: the page size must be between 1 and %d turns", MaxPage)
	}
	if err := s.checkOwner(ctx, scope, sessionID); err != nil {
		return SegmentPage{}, err
	}
	target := forSession(scope, sessionID)

	// A turn is at most two rows, so 2*limit+1 rows hold limit whole turns and
	// tell whether another row follows them.
	rows, err := s.tr.Transcript(ctx, target, cursor, 2*limit+1)
	if err != nil {
		return SegmentPage{}, err
	}
	page := SegmentPage{SessionID: sessionID, Turns: make([]Turn, 0, len(rows))}
	turns, last, i := 0, int64(-1), 0
	for ; i < len(rows); i++ {
		if rows[i].N != last {
			if turns == limit {
				break
			}
			turns++
			last = rows[i].N
		}
		r := rows[i]
		body, withheld := s.display(r.Role, r.Body)
		page.Turns = append(page.Turns, Turn{
			TurnID: r.TurnID, N: r.N, Role: r.Role, Act: r.Act, Body: body, Clipped: r.Clipped, CreatedAt: r.CreatedAt.UTC(), Withheld: withheld,
		})
	}
	if i < len(rows) {
		page.NextCursor = last
	}
	if page.Total, err = s.tr.TranscriptLen(ctx, target); err != nil {
		return SegmentPage{}, err
	}
	return page, nil
}
