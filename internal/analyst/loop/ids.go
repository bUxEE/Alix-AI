package loop

import (
	"fmt"
	"sync/atomic"
)

type counterIDs struct{ n atomic.Uint64 }

func (c *counterIDs) New(prefix string) string {
	v := c.n.Add(1)
	return fmt.Sprintf("%s_%d", prefix, v)
}

func defaultIDs() IDSource { return &counterIDs{} }
