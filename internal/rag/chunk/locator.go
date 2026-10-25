package chunk

import "github.com/buxee/alix-ai/pkg/contracts/gen/conversation"

// Locator is where the chunk sits in its source, as a citation needs it
// (12.5): the page it starts on, the section it is under and its span. The
// span is in code points (see the package comment). A chunk always has a span,
// so the locator always has at least one member, as the schema requires.
func (c Chunk) Locator() conversation.CitationLocator {
	loc := conversation.CitationLocator{
		Span: &conversation.CitationSpan{Start: int64(c.Span.Start), End: int64(c.Span.End)},
	}
	if c.Page > 0 {
		page := int64(c.Page)
		loc.Page = &page
	}
	if c.Section != "" {
		section := truncateRunes(c.Section, maxTitleRunes)
		loc.Section = &section
	}
	return loc
}
