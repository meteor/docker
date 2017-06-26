// +build linux

// Package journald provides the log driver for forwarding server logs
// to endpoints that receive the systemd format.
package journald

import (
	"fmt"
	"strconv"
	"sync"
	"time"
	"unicode"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/go-systemd/journal"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/loggerutils"
	multierror "github.com/hashicorp/go-multierror"
)

const name = "journald"

type journald struct {
	vars    map[string]string // additional variables and values to send to the journal along with the log message
	eVars   map[string]string // vars, plus an extra one saying DOCKER_EVENT=true
	readers readerList

	rateLimitMu     sync.Mutex
	stdoutRateLimit *rateLimit
	stderrRateLimit *rateLimit
}

type readerList struct {
	mu      sync.Mutex
	readers map[*logger.LogWatcher]*logger.LogWatcher
}

func init() {
	if err := logger.RegisterLogDriver(name, New); err != nil {
		logrus.Fatal(err)
	}
	if err := logger.RegisterLogOptValidator(name, validateLogOpt); err != nil {
		logrus.Fatal(err)
	}
}

// sanitizeKeyMode returns the sanitized string so that it could be used in journald.
// In journald log, there are special requirements for fields.
// Fields must be composed of uppercase letters, numbers, and underscores, but must
// not start with an underscore.
func sanitizeKeyMod(s string) string {
	n := ""
	for _, v := range s {
		if 'a' <= v && v <= 'z' {
			v = unicode.ToUpper(v)
		} else if ('Z' < v || v < 'A') && ('9' < v || v < '0') {
			v = '_'
		}
		// If (n == "" && v == '_'), then we will skip as this is the beginning with '_'
		if !(n == "" && v == '_') {
			n += string(v)
		}
	}
	return n
}

// Returns a rateLimit for the container if appropriate labels are set. Returns
// nil if labels are not set or cannot be parsed. Logs errors if labels cannot
// be parsed.
func newRateLimit(labels map[string]string) *rateLimit {
	burstLabel, burstExists := labels["com.meteor.galaxy.log-burst"]
	intervalLabel, intervalExists := labels["com.meteor.galaxy.log-interval"]

	if !burstExists && !intervalExists {
		return nil
	}
	if !burstExists || !intervalExists {
		logrus.Errorf("only one com.meteor.galaxy.log-* label exists: %v %v",
			burstExists, intervalExists)
		return nil
	}

	burst, err := strconv.Atoi(burstLabel)
	if err != nil {
		logrus.Errorf("Couldn't parse com.meteor.galaxy.log-burst '%s': %v",
			burstLabel, err)
		return nil
	}

	interval, err := time.ParseDuration(intervalLabel)
	if err != nil {
		logrus.Errorf("Couldn't parse com.meteor.galaxy.log-interval '%s': %v",
			intervalLabel, err)
		return nil
	}

	return &rateLimit{Burst: burst, Interval: interval}
}

// New creates a journald logger using the configuration passed in on
// the context.
func New(info logger.Info) (logger.Logger, error) {
	if !journal.Enabled() {
		return nil, fmt.Errorf("journald is not enabled on this host")
	}

	// parse log tag
	tag, err := loggerutils.ParseLogTag(info, loggerutils.DefaultTemplate)
	if err != nil {
		return nil, err
	}

	vars := map[string]string{
		"CONTAINER_ID":      info.ContainerID[:12],
		"CONTAINER_ID_FULL": info.ContainerID,
		"CONTAINER_NAME":    info.Name(),
		"CONTAINER_TAG":     tag,
	}
	extraAttrs, err := info.ExtraAttributes(sanitizeKeyMod)
	if err != nil {
		return nil, err
	}
	for k, v := range extraAttrs {
		vars[k] = v
	}

	eVars := map[string]string{"DOCKER_EVENT": "true"}
	for k, v := range vars {
		eVars[k] = v
	}

	return &journald{
		vars:            vars,
		eVars:           eVars,
		readers:         readerList{readers: make(map[*logger.LogWatcher]*logger.LogWatcher)},
		stdoutRateLimit: newRateLimit(info.ContainerLabels),
		stderrRateLimit: newRateLimit(info.ContainerLabels),
	}, nil
}

// We don't actually accept any options, but we have to supply a callback for
// the factory to pass the (probably empty) configuration map to.
func validateLogOpt(cfg map[string]string) error {
	for key := range cfg {
		switch key {
		case "labels":
		case "env":
		case "env-regex":
		case "tag":
		default:
			return fmt.Errorf("unknown log opt '%s' for journald log driver", key)
		}
	}
	return nil
}

type msg struct {
	line     string
	priority journal.Priority
	vars     map[string]string
}

func (s *journald) Log(msg *logger.Message) error {
	vars := map[string]string{}
	for k, v := range s.vars {
		vars[k] = v
	}
	if msg.Partial {
		vars["CONTAINER_PARTIAL_MESSAGE"] = "true"
	}

	line := string(msg.Line)
	source := msg.Source
	logger.PutMessage(msg)

	if source == "event" {
		// Galaxy-specific change! If this is an "event" (container start or stop),
		// send it with the special DOCKER_EVENT=true field. Also, use a distinct
		// priority level from stdout/stderr, since different priority levels are
		// rate limited separately by journald (though this is undocumented) and we
		// don't want a spammy container to cause journald to drop the stop message
		// if our internal rate limiting was ineffective.
		// https://github.com/systemd/systemd/blob/e5e0cffce784b2cf6f57f110cc9c4355f7703200/src/journal/journald-rate-limit.c#L39-L42
		return journal.Send(line, journal.PriWarning, s.eVars)
	}

	var errs error
	for _, msg := range s.msgsForLine(line, source, vars) {
		if err := journal.Send(msg.line, msg.priority, msg.vars); err != nil {
			errs = multierror.Append(errs, err)
		}
	}
	return errs
}

// Returns the list of messages that need to be sent to the journal: probably
// just one describing the message passed in, but maybe messages mentioning that
// lines have been dropped, or missing the one for the message because it should
// be dropped.
//
// This function does not actually send the messages and shouldn't block inside
// the mutex.
func (s *journald) msgsForLine(line, source string, vars map[string]string) []*msg {
	// This method is accessed by two goroutines, one for stdout and one for stderr.
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()

	var msgs []*msg

	// Check rate limits on *both* streams.  We check on both to help the
	// following situation:
	//
	// - Tons of quick writes to stdout that get rate limited
	// - A long time passes, during which there are periodic non-rate-limited
	//   writes to stderr
	// - Eventually, another write to stdout
	//
	// We want the rate limit message to show up as soon as possible, but for
	// simplicity we only print messages when a method is invoked on this driver
	// (Log or Close) rather than trying to manage extra timer
	// goroutines. Checking both rate limits here means that we'll write the
	// "stdout got rate limited" message as soon as the first message to stderr
	// (after the interval expires) happens.
	//
	// Note that it's OK to call MaybeFinishInterval and CanSend on nil pointers.
	if suppressed := s.stdoutRateLimit.MaybeFinishInterval(); suppressed != 0 {
		msgs = append(msgs, &msg{s.makeSuppressedMessage(suppressed, "stdout"), journal.PriWarning, s.eVars})
	}
	if suppressed := s.stderrRateLimit.MaybeFinishInterval(); suppressed != 0 {
		msgs = append(msgs, &msg{s.makeSuppressedMessage(suppressed, "stderr"), journal.PriWarning, s.eVars})
	}

	if source == "stderr" && s.stderrRateLimit.CanSend() {
		msgs = append(msgs, &msg{line, journal.PriErr, vars})
	}
	if source == "stdout" && s.stdoutRateLimit.CanSend() {
		msgs = append(msgs, &msg{line, journal.PriInfo, vars})
	}

	return msgs
}

// Send a DOCKER_EVENT message describing the suppression. 'source' must be safe
// for unquoted insertion into a JSON string.
func (s *journald) sendSuppressedMessage(suppressed int, source string) error {
	return journal.Send(s.makeSuppressedMessage(suppressed, source), journal.PriWarning, s.eVars)
}

// Returns the message used for a DOCKER_EVENT message describing the
// suppression. 'source' must be safe for unquoted insertion into a JSON string.
func (s *journald) makeSuppressedMessage(suppressed int, source string) string {
	return fmt.Sprintf(`{"type":"dropped","lines":%d,"source":"%s"}`, suppressed, source)
}

func (s *journald) finalSuppressMessage(rl *rateLimit, source string) {
	if rl == nil {
		return
	}
	s.rateLimitMu.Lock()
	suppressed := rl.Suppressed()
	s.rateLimitMu.Unlock()

	if suppressed == 0 {
		return
	}

	if err := s.sendSuppressedMessage(suppressed, source); err != nil {
		logrus.Errorf("Couldn't send final suppressed message: %v", err)
	}
}

func (s *journald) Name() string {
	return name
}
