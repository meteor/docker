package journald

import "time"

// rateLimit allows us to rate limit logs coming from a container before they
// are sent to journald rather than after. While journald does its own rate
// limiting, it has a single rate limiter for the entire docker service, so a
// spammy container would cause us to lose logs from a less spammy container.
// The implementation of this type is inspired by journald's
// journal_rate_limit_test.
type rateLimit struct {
	// Number of messages to allow in each interval.
	Burst int
	// Length of interval.
	Interval time.Duration

	// Beginning of the current interval.
	begin time.Time
	// Number of messages allowed in the current interval.
	num int
	// Number of messages suppressed in the current interval.
	suppressed int
}

// CanSend returns true if a message should be allowed now. It assumes we very recently
// called MaybeFinishInterval on it.
func (r *rateLimit) CanSend() bool {
	// If we aren't rate limiting, we can always send.
	if r == nil {
		return true
	}

	// Are we not in an interval? Start one.  Note that in this case we must
	// always have r.suppressed = r.num = 0, because that's how a rateLimit starts
	// and that's what MaybeFinishInterval resets it to when it zeros r.begin.
	if r.begin.IsZero() {
		r.begin = time.Now()
	}

	// Now we're in an interval. Because the caller just called
	// MaybeFinishInterval, the interval isn't done yet. So just check to see if
	// we have room for another message or not.
	if r.num < r.Burst {
		r.num++
		return true
	}

	// Too many within the interval!
	r.suppressed++
	return false
}

// MaybeFinishInterval checks to see if the rateLimit has left its rate-limiting
// interval. If so, it resets the count of suppressed messages and tells you how
// many have been suppressed. It doesn't tell you if you should rate limit right
// now.
func (r *rateLimit) MaybeFinishInterval() int {
	// If we aren't rate limiting, we never suppress.
	if r == nil {
		return 0
	}

	// We can only be in an interval if r.begin is non-zero.
	if r.begin.IsZero() {
		return 0
	}

	// If we're still in the interval, it's not time to send a dropped-lines message yet.
	if !r.begin.Add(r.Interval).Before(time.Now()) {
		return 0
	}

	// We have left an interval, so let's send a dropped-lines message and
	// register that we're no longer in an interval.
	previousSuppressed := r.suppressed
	r.suppressed = 0
	r.num = 0
	r.begin = time.Time{}
	return previousSuppressed
}

// Returns the number of currently suppressed messages. It doesn't make a
// difference if the interval has expired. Intended for use at the end of a
// stream.
func (r *rateLimit) Suppressed() int {
	return r.suppressed
}
