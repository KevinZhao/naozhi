package front

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/textutil"
)

// The ledger's open items (未了事项, docs/rfc/planner.md §15.23, D143): the
// concrete things left undone on the user's lines of work, one sentence
// each. They are collected, not typed in: a dispatched session's report
// leaves them, the Planner writes the ones it reads of, the patrol's checks
// file their hits. An item closes on a fact where its source has one (the
// PR merged, the issue closed, the session reporting done, the check no
// longer hitting); the rest are closed by the Planner or struck by the
// user. They live in fronts.json beside the fronts, one atomic write for
// both.

// Bounds of the items.
const (
	// ItemRunes caps an item's text, SourceRunes its source.
	ItemRunes   = 100
	SourceRunes = 300
	// MaxOpenItems caps the open items: past what one person reads, the
	// list is no longer a place to look.
	MaxOpenItems = 300
	// itemRetention is how long a closed item is kept: long enough to see
	// who closed it, and for Reconcile to know the user struck it.
	itemRetention = 7 * 24 * time.Hour
	// itemIDLen is the random part of an item's ID.
	itemIDLen = 7
	// workspaceRunes caps the workspace ID an item names.
	workspaceRunes = 128
)

// From is where an item came from.
type From string

// The sources of an item.
const (
	// FromReport is the left of a dispatched session's report block.
	FromReport From = "report"
	// FromPatrol is the project patrol (D142).
	FromPatrol From = "patrol"
	// FromPlanner is the Planner's own judgment, in a turn the user did not
	// start.
	FromPlanner From = "planner"
	// FromUser is what the user said, or added in the left column.
	FromUser From = "user"
)

// Who closed an item (Item.ClosedBy).
const (
	// ByUser is the user striking it, or a front block's done in a turn
	// the user started. It alone keeps the item from being filed again
	// (reconcile).
	ByUser = "user"
	// ByPlanner is a front block's done in any other turn.
	ByPlanner = "planner"
	// ByReport is its session's next report not listing it, or reporting
	// done with nothing left.
	ByReport = "report"
	// ByFact is its PR merged or closed, its issue closed.
	ByFact = "fact"
	// ByPatrol is a deterministic check no longer hitting.
	ByPatrol = "patrol"
)

// Item is one open item. Source is free text; the gateway knows a few
// shapes of it (SourcePR, SourceIssue, a session key, the patrol's
// "check:…" and "patrol:…") and shows the rest as written.
type Item struct {
	// ID is the store's: "i" and seven of [a-z0-9].
	ID string `json:"id"`
	// Text is the thing left, on one line.
	Text string `json:"text"`
	// Workspace is the workspace ID it belongs to, "" when unknown.
	Workspace string `json:"workspace,omitempty"`
	// Front is the line it was filed under, "" for none. Items resolves the
	// line it shows under.
	Front string `json:"front,omitempty"`
	// Source is where it came from, for the user to open and for the
	// gateway to close it by.
	Source  string    `json:"source,omitempty"`
	From    From      `json:"from"`
	Created time.Time `json:"created"`
	// Closed is when it was closed, zero while open; ClosedBy by whom.
	Closed   time.Time `json:"closed,omitzero"`
	ClosedBy string    `json:"closed_by,omitempty"`
}

// Open reports whether it is still left.
func (it Item) Open() bool { return it.Closed.IsZero() }

// The ways a write of an item fails, in the words the Planner's receipt
// uses.
var (
	// ErrItemsFull is a new item past MaxOpenItems.
	ErrItemsFull = fmt.Errorf("开着的未了事项已有 %d 条，先了结一些", MaxOpenItems)
	// ErrNoItem is an ID no item has.
	ErrNoItem = errors.New("找不到这条未了事项（id 见 fronts.json 的 items）")
	// ErrItemClosed is an edit of an item already closed.
	ErrItemClosed = errors.New("这条未了事项已经了结")
	// ErrNoFront is an item filed under a line the ledger does not have.
	ErrNoFront = errors.New("没有这条工作线")
	// ErrPatrolSource is a new source for an item the patrol filed.
	ErrPatrolSource = errors.New("巡查记的未了事项按出处认，出处不能改；要换出处就用 done 了结它，再记一条新的")
)

// PatrolSource opens the source of the item a deep check's finding is
// filed as, "patrol:<workspace ID>/<finding ID>" (planner/patrol).
const PatrolSource = "patrol:"

var itemIDRE = regexp.MustCompile(`^i[a-z0-9]{7}$`)

// ItemID reports whether s has the shape of an item's ID.
func ItemID(s string) bool { return itemIDRE.MatchString(s) }

// oneLine is s with its white space folded: an item is one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func checkText(text string) error {
	switch n := utf8.RuneCountInString(text); {
	case n == 0:
		return errors.New("未了事项缺少 text")
	case n > ItemRunes:
		return fmt.Errorf("未了事项的 text 超过 %d 字", ItemRunes)
	}
	return nil
}

func checkSource(source string) error {
	if utf8.RuneCountInString(source) > SourceRunes {
		return fmt.Errorf("未了事项的 source 超过 %d 字", SourceRunes)
	}
	return nil
}

// normalize folds it's fields as the ledger keeps them and reports what is
// wrong with it.
func (it *Item) normalize() error {
	it.Text, it.Source = oneLine(it.Text), oneLine(it.Source)
	it.Workspace, it.Front = strings.TrimSpace(it.Workspace), strings.TrimSpace(it.Front)
	if err := checkText(it.Text); err != nil {
		return err
	}
	if err := checkSource(it.Source); err != nil {
		return err
	}
	if utf8.RuneCountInString(it.Workspace) > workspaceRunes {
		return errors.New("未了事项的 workspace 太长")
	}
	switch it.From {
	case FromReport, FromPatrol, FromPlanner, FromUser:
	default:
		return fmt.Errorf("未了事项的来源 %q 不认识", it.From)
	}
	return nil
}

// AddItem files it (Text, Workspace, Front, Source and From; the rest is
// the store's). An open item the same in all five is returned as it is,
// added false: a block run twice, or a click sent twice, leaves one item.
// The same sentence under another line or workspace is another item: the
// user adding it there would otherwise see nothing appear. Front must name
// a line the ledger has.
func (s *Store) AddItem(it Item) (out Item, added bool, err error) {
	if err := it.normalize(); err != nil {
		return Item{}, false, err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if it.Front != "" && !s.hasLocked(it.Front) {
		return Item{}, false, ErrNoFront
	}
	if i := slices.IndexFunc(s.items, func(o Item) bool {
		return o.Open() && o.From == it.From && o.Source == it.Source && o.Text == it.Text &&
			o.Front == it.Front && o.Workspace == it.Workspace
	}); i >= 0 {
		return s.items[i], false, nil
	}
	if s.openItemsLocked() >= MaxOpenItems {
		return Item{}, false, ErrItemsFull
	}
	out = s.addLocked(it, now)
	s.saveLocked(now)
	return out, true, nil
}

// addLocked appends it, normalized, as a new open item.
func (s *Store) addLocked(it Item, now time.Time) Item {
	it.ID, it.Created, it.Closed, it.ClosedBy = s.newItemIDLocked(), now, time.Time{}, ""
	s.items = append(s.items, it)
	return it
}

const itemIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func (s *Store) newItemIDLocked() string {
	for {
		b := make([]byte, itemIDLen)
		for i := range b {
			b[i] = itemIDAlphabet[rand.IntN(len(itemIDAlphabet))]
		}
		id := "i" + string(b)
		if !slices.ContainsFunc(s.items, func(o Item) bool { return o.ID == id }) {
			return id
		}
	}
}

func (s *Store) openItemsLocked() int {
	n := 0
	for _, it := range s.items {
		if it.Open() {
			n++
		}
	}
	return n
}

func (s *Store) hasLocked(front string) bool {
	return slices.ContainsFunc(s.fronts, func(f Front) bool { return f.ID == front })
}

// EditItem changes an open item's text, source or line; nil leaves a field,
// a front of "" takes it off its line. The ID and Created stay: an item
// whose source becomes the issue it was filed as is still that item.
//
// An item the patrol filed keeps its source (ErrPatrolSource): the source
// is what its check, or its finding's record, tells it by. Given another it
// would leave its group, the check still hitting would file a second item,
// and nothing would close the first.
func (s *Store) EditItem(id string, text, source, front *string) (Item, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.items, func(o Item) bool { return o.ID == id })
	switch {
	case i < 0:
		return Item{}, ErrNoItem
	case !s.items[i].Open():
		return Item{}, ErrItemClosed
	}
	it := s.items[i]
	if text != nil {
		it.Text = oneLine(*text)
		if err := checkText(it.Text); err != nil {
			return Item{}, err
		}
	}
	if source != nil {
		it.Source = oneLine(*source)
		if err := checkSource(it.Source); err != nil {
			return Item{}, err
		}
		if it.From == FromPatrol && it.Source != s.items[i].Source {
			return Item{}, ErrPatrolSource
		}
	}
	if front != nil {
		if it.Front = strings.TrimSpace(*front); it.Front != "" && !s.hasLocked(it.Front) {
			return Item{}, ErrNoFront
		}
	}
	if it != s.items[i] {
		s.items[i] = it
		s.saveLocked(now)
	}
	return it, nil
}

// CloseItem closes the item id names, by being who (the By constants). One
// already closed is returned as it was closed, but for the user striking
// one somebody else closed: the page they struck it on may not have seen it
// close, and what they said (not to be filed again, reconcile) holds from
// now.
func (s *Store) CloseItem(id, by string) (Item, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.items, func(o Item) bool { return o.ID == id })
	if i < 0 {
		return Item{}, ErrNoItem
	}
	if it := &s.items[i]; it.Open() || by == ByUser && it.ClosedBy != ByUser {
		it.Closed, it.ClosedBy = now, by
		s.saveLocked(now)
	}
	return s.items[i], nil
}

// CloseItems closes every open item match takes, in one write, and returns
// them. match runs under the store's lock: it must not call the store.
func (s *Store) CloseItems(match func(Item) bool, by string) []Item {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var closed []Item
	for i := range s.items {
		if it := &s.items[i]; it.Open() && match(*it) {
			it.Closed, it.ClosedBy = now, by
			closed = append(closed, *it)
		}
	}
	if len(closed) > 0 {
		s.saveLocked(now)
	}
	return closed
}

// Reconcile makes the open items match takes be exactly want, told apart by
// Source, in one write: a wanted one already open keeps its ID and Created
// and takes the new text; one not open yet is filed; an open one no longer
// wanted is closed by by. match runs under the store's lock: it must not
// call the store. What is left out of want, and why, is reconcile's.
//
// It is what a deterministic check calls each round with its hits (the
// patrol, D142): the hits are the list, whatever was filed before.
func (s *Store) Reconcile(match func(Item) bool, want []Item, by string) (added, closed []Item) {
	added, closed, _ = s.reconcile(match, want, by, func(it Item) string { return it.Source })
	return added, closed
}

// ReconcileText is Reconcile for a list told apart by its items' text: what
// one source says is left, said whole each time (a dispatched session's
// report, whose items all have the session for their source). One said
// again keeps its ID; one no longer said is closed by by. full is how many
// were not filed for MaxOpenItems.
func (s *Store) ReconcileText(match func(Item) bool, want []Item, by string) (added, closed []Item, full int) {
	return s.reconcile(match, want, by, func(it Item) string { return it.Text })
}

// reconcile is Reconcile and ReconcileText, key what tells two items of the
// group apart.
//
// A wanted one the user struck (an item of the group with the same From,
// Source and key closed by ByUser, still kept) is not filed again: the user
// said no, and the check hitting again an hour later is not news. Only a
// strike within the group counts: the same source in another group (the
// same check in another workspace) is another item.
//
// A wanted item's text past ItemRunes is cut. One that does not validate,
// has no key or is not one match takes is left out (filed outside its own
// group it would be filed again every round), as is one past MaxOpenItems;
// its key still counts as wanted, so the item it names stays open: what
// could not be filed this round has not stopped hitting. A wanted item's
// Front the ledger lacks is dropped, the item kept.
func (s *Store) reconcile(match func(Item) bool, want []Item, by string, key func(Item) string) (added, closed []Item, full int) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := map[string]bool{}
	var valid []Item
	for _, w := range want {
		if w.Text = oneLine(w.Text); utf8.RuneCountInString(w.Text) > ItemRunes {
			w.Text = textutil.TruncateRunesNoEllipsis(w.Text, ItemRunes-1) + "…"
		}
		err := w.normalize()
		k := key(w)
		switch {
		case k == "":
			err = cmp.Or(err, errors.New("nothing to tell it apart by"))
		case err == nil && !match(w):
			err = errors.New("not in the group reconciled")
		}
		if k != "" {
			wanted[k] = true
		}
		if err != nil {
			slog.Warn("planner fronts: an item to reconcile left out", "source", w.Source, "err", err)
			continue
		}
		if w.Front != "" && !s.hasLocked(w.Front) {
			w.Front = ""
		}
		valid = append(valid, w)
	}
	// Closed before the new ones are filed: at the cap, a list that swaps
	// its items still fits.
	for i := range s.items {
		if it := &s.items[i]; it.Open() && !wanted[key(*it)] && match(*it) {
			it.Closed, it.ClosedBy = now, by
			closed = append(closed, *it)
		}
	}
	changed := false
	for _, w := range valid {
		same := func(o Item) bool { return key(o) == key(w) && match(o) }
		if i := slices.IndexFunc(s.items, func(o Item) bool { return o.Open() && same(o) }); i >= 0 {
			if s.items[i].Text != w.Text {
				s.items[i].Text = w.Text
				changed = true
			}
			continue
		}
		if slices.ContainsFunc(s.items, func(o Item) bool {
			return !o.Open() && o.ClosedBy == ByUser && o.From == w.From && o.Source == w.Source && same(o) &&
				now.Sub(o.Closed) <= itemRetention
		}) {
			continue
		}
		if s.openItemsLocked() >= MaxOpenItems {
			slog.Warn("planner fronts: open items at their cap; an item to reconcile left out", "source", w.Source)
			full++
			continue
		}
		added = append(added, s.addLocked(w, now))
	}
	if changed || len(added)+len(closed) > 0 {
		s.saveLocked(now)
	}
	return added, closed, full
}

// Items lists the open items, the oldest first so the list does not jump,
// each with Front the open line it shows under, "" for none: the line it
// was filed under while that is open, else the open line whose sessions
// hold its source (the Planner often puts a session on a line after the
// session's report filed the item). The line is worked out here and never
// written back, so an item of a line that closed shows under no line and
// goes back under it when the line reopens.
func (s *Store) Items() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	open := map[string]bool{}
	bySession := map[string]string{}
	var latest map[string]time.Time
	for _, f := range s.fronts {
		if !f.Open() {
			continue
		}
		open[f.ID] = true
		for _, k := range f.Sessions {
			// A session on two lines goes by the one updated last.
			if at, ok := latest[k]; !ok || f.Updated.After(at) {
				if latest == nil {
					latest = map[string]time.Time{}
				}
				latest[k], bySession[k] = f.Updated, f.ID
			}
		}
	}
	var out []Item
	for _, it := range s.items {
		if !it.Open() {
			continue
		}
		if !open[it.Front] {
			it.Front = bySession[it.Source]
		}
		out = append(out, it)
	}
	slices.SortStableFunc(out, func(a, b Item) int { return a.Created.Compare(b.Created) })
	return out
}

// Sources the gateway closes an item by. Both take the canonical GitHub
// URL only: the parts go to gh as arguments, so nothing else may ride in.
var (
	prRE    = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([1-9][0-9]{0,8})(?:[/?#].*)?$`)
	issueRE = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/issues/([1-9][0-9]{0,8})(?:[/?#].*)?$`)
	hashRE  = regexp.MustCompile(`^#([1-9][0-9]{0,8})$`)
)

// SourcePR is the GitHub pull request source names by its URL.
func SourcePR(source string) (owner, repo string, n int, ok bool) {
	return sourceURL(prRE, source)
}

// SourceIssue is the GitHub issue source names: by its URL, or as "#n" of
// the repository the item's workspace is in (owner and repo then "").
func SourceIssue(source string) (owner, repo string, n int, ok bool) {
	if m := hashRE.FindStringSubmatch(strings.TrimSpace(source)); m != nil {
		n, _ = strconv.Atoi(m[1])
		return "", "", n, true
	}
	return sourceURL(issueRE, source)
}

func sourceURL(re *regexp.Regexp, source string) (owner, repo string, n int, ok bool) {
	m := re.FindStringSubmatch(strings.TrimSpace(source))
	if m == nil || strings.Trim(m[1], ".") == "" || strings.Trim(m[2], ".") == "" {
		return "", "", 0, false
	}
	n, _ = strconv.Atoi(m[3])
	return m[1], m[2], n, true
}
