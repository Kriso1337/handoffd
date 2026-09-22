package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/outbox"
	"github.com/Kriso1337/handoffd/internal/session"
)

const (
	postWaitDefault = 60 * time.Second
	postPoll        = 500 * time.Millisecond
	exitRejected    = 1
	exitPending     = 2
)

var errPostPending = errors.New("post pending")

type postOptions struct {
	record outbox.Record
	wait   time.Duration
}

func parsePost(args []string, sessionID string, now time.Time) (postOptions, error) {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	thread := fs.String("thread", "", "thread root ts")
	mrNote := fs.Bool("mr-note", false, "comment in the MR of the thread")
	marker := fs.String("marker", "", "taken | review-start")
	reviewDone := fs.Bool("review-done", false, "submit the review result")
	sha := fs.String("sha", "", "commit the message is about")
	blockers := fs.Int("blockers", 0, "blocking findings")
	others := fs.Int("others", 0, "other findings")
	decision := fs.String("decision", "", "decision needed from a human")
	wait := fs.Duration("wait", postWaitDefault, "how long to wait for the receipt")
	if err := fs.Parse(args); err != nil {
		return postOptions{}, err
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	rec := outbox.Record{ID: outbox.NewID(), At: now, ThreadTS: *thread, SHA: *sha, SessionID: sessionID, Text: text}
	switch {
	case *mrNote && *marker == "" && !*reviewDone:
		rec.Kind = outbox.KindMRNote
	case *marker != "" && !*mrNote && !*reviewDone:
		rec.Kind = outbox.KindMarker
		switch *marker {
		case "taken":
			rec.Phase = outbox.PhaseTaken
		case "review-start":
			rec.Phase = outbox.PhaseReviewStart
		default:
			return postOptions{}, fmt.Errorf("marker %q is not taken or review-start", *marker)
		}
	case *reviewDone && !*mrNote && *marker == "":
		rec.Kind = outbox.KindReviewDone
		rec.Blockers, rec.Others, rec.Decision = *blockers, *others, *decision
	case !*mrNote && *marker == "" && !*reviewDone:
		rec.Kind = outbox.KindReply
	default:
		return postOptions{}, errors.New("choose one of --mr-note, --marker, --review-done or plain text")
	}
	if sessionID == "" {
		return postOptions{}, errNoSession
	}
	if err := rec.Validate(); err != nil {
		return postOptions{}, err
	}
	return postOptions{record: rec, wait: *wait}, nil
}

var errNoSession = fmt.Errorf("no session: set %s, run `handoffd link` in this window, or open it through the watcher", session.EnvSessionID)

func post(cfg config.Config, configPath string, args []string, out io.Writer) error {
	opts, err := parsePost(args, os.Getenv(session.EnvSessionID), time.Now().UTC())
	if errors.Is(err, errNoSession) {
		var linked string
		if linked, err = linkedSession(cfg, configPath); err == nil {
			opts, err = parsePost(args, linked, time.Now().UTC())
		}
	}
	if err != nil {
		return err
	}
	dir := outbox.New(outboxPath(cfg))
	if _, err := dir.Write(opts.record); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.wait+postPoll)
	defer cancel()
	rc, ok, err := outbox.Wait(ctx, dir, opts.record.ID, opts.wait, postPoll)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if !ok {
		fmt.Fprintf(out, "pending %s: no receipt within %s, the watcher delivers or dead-letters it\n", opts.record.ID, opts.wait)
		return errPostPending
	}
	switch rc.Status {
	case outbox.StatusPosted:
		fmt.Fprintf(out, "posted %s\n", rc.Permalink)
		return nil
	case outbox.StatusRejected:
		return fmt.Errorf("rejected: %s", rc.Reason)
	default:
		fmt.Fprintf(out, "pending %s: %s\n", opts.record.ID, rc.Reason)
		return errPostPending
	}
}

func outboxPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.StatePath), "outbox")
}
