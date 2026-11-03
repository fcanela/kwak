package listener

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

const (
	syncTimeout  = 30 * time.Second
	syncDeadline = 60 * time.Second
	pageLimit    = 100
	backfillFile = "backfill.jsonl"
	pageFilter   = `{"types":["m.room.message"]}`
)

// syncFilter is the filter of spec 5.3.
func syncFilter(room string) string {
	b, _ := json.Marshal(map[string]any{
		"presence":     map[string]any{"types": []string{}},
		"account_data": map[string]any{"types": []string{}},
		"room": map[string]any{
			"rooms":        []string{room},
			"account_data": map[string]any{"types": []string{}},
			"ephemeral":    map[string]any{"types": []string{}},
			"state":        map[string]any{"types": []string{}, "lazy_load_members": true},
			"timeline":     map[string]any{"limit": pageLimit, "types": []string{"m.room.message"}},
		},
	})
	return string(b)
}

var (
	errUnknownToken = errors.New("listener: the standing token is no longer valid")
	errHalted       = errors.New("listener: sync stopped while unpaired")
)

// syncLoop long-polls /sync until ctx ends or the standing token stops working (spec 5.3).
func (l *Listener) syncLoop(ctx context.Context, token string) error {
	m := l.d.Matrix(token)
	fails := 0
	for ctx.Err() == nil {
		err := l.syncStep(ctx, m)
		if err == nil {
			fails = 0
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if matrix.IsCode(err, matrix.ErrUnknownToken) {
			return errUnknownToken
		}
		if errors.Is(err, errHalted) {
			return err
		}
		wait := l.backoff(fails)
		fails++
		var me *matrix.Error
		if errors.As(err, &me) && me.Code == matrix.ErrLimit && me.RetryAfter > 0 {
			wait = me.RetryAfter
		}
		l.mu.Lock()
		l.syncErr = "sync failed: " + err.Error()
		l.mu.Unlock()
		l.log.Warn("sync failed", "err", err)
		select {
		case <-ctx.Done():
		case <-l.d.Clock.After(wait):
		}
	}
	return ctx.Err()
}

// syncStep runs one /sync, a gap fill when needed, and routes what arrived.
func (l *Listener) syncStep(ctx context.Context, m Matrix) error {
	l.mu.Lock()
	room, since, pending := l.roomID(), l.st.NextBatch, l.st.Backfill != nil
	l.mu.Unlock()
	if pending {
		if err := l.backfillRun(ctx, m, room); err != nil {
			return err
		}
		l.mu.Lock()
		since = l.st.NextBatch
		l.mu.Unlock()
	}
	sctx, cancel := context.WithTimeout(ctx, syncDeadline)
	resp, err := m.Sync(sctx, since, syncFilter(room), syncTimeout)
	cancel()
	if err != nil {
		return err
	}
	now := l.d.Clock.Now()
	l.mu.Lock()
	if l.phase == wire.StateUnpaired {
		l.mu.Unlock()
		return errHalted
	}
	l.phase, l.syncErr, l.lastSync = wire.StateSyncing, "", now
	if since == "" {
		l.st.NextBatch, l.st.LastEventTS = resp.NextBatch, now.UnixMilli()
		err := l.saveLocked()
		l.mu.Unlock()
		return err
	}
	last, lastTS := l.st.LastEventID, l.st.LastEventTS
	l.mu.Unlock()
	tl := resp.Rooms.Join[room].Timeline
	evs := tl.Events
	for i := range evs {
		evs[i].RoomID = room
	}
	if tl.Limited && tl.PrevBatch != "" && !reached(evs, last, lastTS) {
		if err := l.startBackfill(evs, tl.PrevBatch, resp.NextBatch); err != nil {
			return err
		}
		return l.backfillRun(ctx, m, room)
	}
	l.routeEvents(ctx, m, after(evs, last))
	return l.commit(resp.NextBatch, evs)
}

// reached reports whether evs reach back to the last event already routed.
func reached(evs []matrix.Event, last string, lastTS int64) bool {
	for _, e := range evs {
		if e.EventID == last || (last == "" && e.TS < lastTS) {
			return true
		}
	}
	return false
}

func after(evs []matrix.Event, last string) []matrix.Event {
	for i, e := range evs {
		if e.EventID == last {
			return evs[i+1:]
		}
	}
	return evs
}

// commit saves next_batch together with the inboxes the batch produced (spec 5.3).
func (l *Listener) commit(next string, evs []matrix.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.st.NextBatch = next
	if n := len(evs); n > 0 {
		l.st.LastEventID, l.st.LastEventTS = evs[n-1].EventID, evs[n-1].TS
	}
	return l.saveLocked()
}

func (l *Listener) backfillPath() string { return filepath.Join(l.dir, backfillFile) }

// startBackfill writes the sync timeline as the newest page and records where paging starts.
func (l *Listener) startBackfill(evs []matrix.Event, prev, next string) error {
	if err := os.WriteFile(l.backfillPath(), nil, 0o600); err != nil {
		return err
	}
	rev := make([]matrix.Event, len(evs))
	for i, e := range evs {
		rev[len(evs)-1-i] = e
	}
	if err := l.appendPage(rev); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.st.Backfill = &backfill{From: prev, NextBatch: next}
	return l.saveLocked()
}

func (l *Listener) appendPage(evs []matrix.Event) error {
	f, err := os.OpenFile(l.backfillPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	data, err := json.Marshal(evs)
	if err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// backfillRun pages back with /messages until it reaches the last routed event, saving each
// page and its end token, then routes the file oldest first (spec 5.3). There is no page cap.
func (l *Listener) backfillRun(ctx context.Context, m Matrix, room string) error {
	for {
		l.mu.Lock()
		bf := *l.st.Backfill
		last, lastTS := l.st.LastEventID, l.st.LastEventTS
		l.mu.Unlock()
		if bf.Done {
			break
		}
		page, err := m.Messages(ctx, room, bf.From, pageLimit, pageFilter)
		if err != nil {
			return err
		}
		var keep []matrix.Event
		hit := false
		for _, e := range page.Chunk {
			if e.EventID == last || e.TS < lastTS {
				hit = true
				break
			}
			e.RoomID = room
			keep = append(keep, e)
		}
		if err := l.appendPage(keep); err != nil {
			return err
		}
		switch {
		case hit:
			bf.Done = true
		case page.End == "" || len(page.Chunk) == 0:
			bf.Done, bf.Missed = true, true
		default:
			bf.From = page.End
		}
		l.mu.Lock()
		l.st.Backfill = &bf
		err = l.saveLocked()
		l.mu.Unlock()
		if err != nil {
			return err
		}
	}
	newest, err := l.routeFile(ctx, m)
	if err != nil {
		return err
	}
	l.mu.Lock()
	bf := l.st.Backfill
	l.st.Backfill = nil
	l.st.NextBatch = bf.NextBatch
	if newest != nil {
		l.st.LastEventID, l.st.LastEventTS = newest.EventID, newest.TS
	}
	if bf.Missed {
		l.missedLocked()
	}
	err = l.saveLocked()
	l.mu.Unlock()
	if err != nil {
		return err
	}
	return os.Remove(l.backfillPath())
}

// routeFile routes backfill.jsonl oldest first, reading one page at a time.
func (l *Listener) routeFile(ctx context.Context, m Matrix) (*matrix.Event, error) {
	f, err := os.Open(l.backfillPath())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var offs []int64
	var pos int64
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			offs = append(offs, pos)
			pos += int64(len(line))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	var newest *matrix.Event
	for i := len(offs) - 1; i >= 0; i-- {
		if _, err := f.Seek(offs[i], io.SeekStart); err != nil {
			return nil, err
		}
		line, err := bufio.NewReader(f).ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		var page []matrix.Event
		if err := json.Unmarshal(line, &page); err != nil {
			return nil, fmt.Errorf("listener: %s: %w", backfillFile, err)
		}
		for j := len(page) - 1; j >= 0; j-- {
			l.routeEvents(ctx, m, page[j:j+1])
			e := page[j]
			newest = &e
		}
	}
	return newest, nil
}

func (l *Listener) missedLocked() {
	now := l.d.Clock.Now()
	for _, s := range l.st.Sessions {
		if s.Ended == nil {
			s.Inbox.add(render.Item{EventID: "kwak-missed-" + fmt.Sprint(now.UnixNano()), SentAt: now,
				Reason: render.Notice, Body: "some messages may have been missed"})
		}
	}
}

// routeEvents routes events in order and queues their deliveries. A thread root that Route
// could not attribute after a state reset is fetched once (spec 5.4).
func (l *Listener) routeEvents(ctx context.Context, m Matrix, evs []matrix.Event) {
	for _, ev := range evs {
		l.mu.Lock()
		ds := Route(ev, l.idx)
		missing := l.idx.Missing
		l.idx.Missing = nil
		l.applyLocked(ev, ds)
		l.mu.Unlock()
		for _, root := range missing {
			re, err := m.Event(ctx, l.roomIDLocked(), root)
			if err != nil {
				continue
			}
			l.mu.Lock()
			if _, mine := l.st.Mine[re.Sender]; mine {
				l.st.Roots[root] = re.Sender
				l.applyLocked(ev, Route(ev, l.idx))
			}
			l.mu.Unlock()
		}
	}
}

// applyLocked queues an event's deliveries and clears the question it answers.
func (l *Listener) applyLocked(ev matrix.Event, ds []Delivery) {
	c, ok := parseContent(ev.Content)
	if !ok {
		return
	}
	if c.relType == relThread {
		if q := l.st.Questions[c.relEvent]; q != nil && q.Asker != ev.Sender {
			delete(l.st.Questions, c.relEvent)
		}
	}
	root := ""
	if c.relType == relThread {
		root = c.relEvent
	}
	for _, d := range ds {
		s := l.st.Sessions[d.Session]
		if s == nil {
			continue
		}
		it := render.Item{EventID: ev.EventID, Sender: ev.Sender, ThreadRoot: root,
			SentAt: time.UnixMilli(ev.TS).UTC(), Reason: d.Reason, Body: c.body}
		q := &s.Inbox
		if d.Helper != "" {
			h := s.Helpers[d.Helper]
			switch {
			case h != nil && h.Bound != "" && !h.Finished:
				q = &h.Inbox
			case h != nil && h.Finished:
				it.Helper, it.Note = d.Helper, "which has finished"
			default:
				it.Helper = d.Helper
			}
		}
		if q.add(it) {
			l.dirty = true
			own := root != "" && l.st.Roots[root] != "" && l.st.Mine[l.st.Roots[root]].Session == d.Session
			l.resumeTriggerLocked(s, d.Reason, own)
		}
	}
}
