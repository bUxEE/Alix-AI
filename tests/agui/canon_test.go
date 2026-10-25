package aguigold

import (
	"fmt"
	"regexp"
	"strings"
)

// canon replaces the random identifiers of a recording by canonical ones, so a recording is the same on every run. An
// identifier is a lower-case prefix, an underscore and a 26-character ULID; it becomes the prefix, the same ULID shape
// made of zeros and a counter, numbered per prefix by the order in which the identifiers first appear across everything
// the same canon is applied to. The ids derived from one (msg_trn_<ULID>_1, alixai_confirm_prep_<ULID>) are replaced
// with it, because the match is on the prefix and the ULID wherever they sit.
type canon struct {
	next map[string]int
	seen map[string]string
}

var canonID = regexp.MustCompile(`([a-z]+)_([0-7][0-9A-HJKMNP-TV-Z]{25})([^0-9A-Za-z]|$)`)

func newCanon() *canon { return &canon{next: map[string]int{}, seen: map[string]string{}} }

func (c *canon) apply(s string) string {
	return canonID.ReplaceAllStringFunc(s, func(m string) string {
		sub := canonID.FindStringSubmatch(m)
		prefix, ulid, tail := sub[1], sub[2], sub[3]
		key := prefix + "_" + ulid
		repl, ok := c.seen[key]
		if !ok {
			c.next[prefix]++
			repl = prefix + "_01J" + strings.Repeat("0", 20) + fmt.Sprintf("%03d", c.next[prefix])
			c.seen[key] = repl
		}
		return repl + tail
	})
}
