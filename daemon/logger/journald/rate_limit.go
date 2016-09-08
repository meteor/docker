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

// Check returns a boolean saying whether or not a message should be allowed
// now, and the number of messages that were suppressed before this message.
func (r *rateLimit) Check() (bool, int) {
	now := time.Now()

	// Is this the first time? Start the interval.
	if r.begin.IsZero() {
		r.suppressed = 0
		r.num = 1
		r.begin = now
		return true, 0
	}

	// Have we left the previous tracked interval? Start a new interval, and maybe
	// report suppression.
	if r.begin.Add(r.Interval).Before(now) {
		previousSuppressed := r.suppressed
		r.suppressed = 0
		r.num = 1
		r.begin = now
		return true, previousSuppressed
	}

	// Within the interval, but within the burst limit?
	if r.num < r.Burst {
		r.num++
		return true, 0
	}

	// Too many within the interval!
	r.suppressed++
	return false, 0
}

// Returns the number of currently suppressed messages.
func (r *rateLimit) Suppressed() int {
	return r.suppressed
}
