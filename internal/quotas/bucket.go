package quotas

import "time"

// tokenUnit is one token in the bucket's integer unit. The level is counted in
// token-nanoseconds divided by the rate: one token is a minute's worth of
// units, and a tenant refilling at R tokens a minute gains exactly R units per
// nanosecond. All arithmetic is integer, so the boundary is exact: the request
// that finds exactly one token passes, with no floating-point rounding to
// decide a close case the wrong way.
const tokenUnit = int64(time.Minute)

// bucket is one tenant's token bucket. It is only touched under Quotas.mu and
// takes its limits as arguments, so a changed limit applies to the next call
// and the bucket carries no copy of it.
type bucket struct {
	level int64
	at    time.Time
	init  bool
}

// refill brings the level up to now, capped at burst. A clock that moves
// backwards adds nothing (and restarts the interval): it can never mint tokens.
func (b *bucket) refill(now time.Time, burst, rate int64) {
	capUnits := burst * tokenUnit
	if !b.init {
		b.level, b.at, b.init = capUnits, now, true
		return
	}
	if el := now.Sub(b.at); el > 0 {
		// Past this many nanoseconds the bucket is full whatever it held, and
		// the cut-off keeps el*rate inside int64.
		if int64(el) >= capUnits/rate+1 {
			b.level = capUnits
		} else {
			b.level += int64(el) * rate
		}
	}
	b.at = now
	if b.level > capUnits {
		b.level = capUnits
	}
}

// take removes n tokens when the bucket holds them and reports ok. When it
// does not, nothing is removed and wait is how long until n tokens exist (the
// bucket is already refilled to now, so a rejection costs nothing).
func (b *bucket) take(now time.Time, burst, rate, n int64) (ok bool, wait time.Duration) {
	b.refill(now, burst, rate)
	need := n * tokenUnit
	if b.level >= need {
		b.level -= need
		return true, 0
	}
	return false, time.Duration((need - b.level + rate - 1) / rate)
}

// full reports whether the bucket would be full at now: a tenant in that state
// can be forgotten, because a fresh bucket is full too.
func (b *bucket) full(now time.Time, burst, rate int64) bool {
	if !b.init {
		return true
	}
	b.refill(now, burst, rate)
	return b.level >= burst*tokenUnit
}

// available is the whole tokens in the bucket at now.
func (b *bucket) available(now time.Time, burst, rate int64) int64 {
	b.refill(now, burst, rate)
	return b.level / tokenUnit
}
