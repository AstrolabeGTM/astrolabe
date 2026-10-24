package views

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/notes"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// PersonScore is a person's score for one product with its one-line reason.
type PersonScore struct {
	score.Score
	Why string `json:"why"`
}

// EmailPolicy says whether the outreach guard lets a product email them.
type EmailPolicy struct {
	Product  string `json:"product"`
	MayEmail bool   `json:"may_email"`
	Reason   string `json:"reason,omitempty"`
	// HasAddress is false when there is no email address at all; email
	// steps are then held as blocked whatever the guard says.
	HasAddress bool `json:"has_address"`
}

type Brief struct {
	Product string    `json:"product"`
	Body    string    `json:"brief"`
	Written time.Time `json:"written"`
}

type Enrollment struct {
	ID         int64      `json:"id"`
	Product    string     `json:"product"`
	Sequence   string     `json:"sequence"`
	Status     string     `json:"status"`
	StopReason string     `json:"stop_reason,omitempty"`
	NextStep   int        `json:"next_step"` // 1-based
	Steps      int        `json:"steps"`
	NextDue    *time.Time `json:"next_due,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	Automation string     `json:"automation,omitempty"`
}

type Message struct {
	ID        int64      `json:"id"`
	Product   string     `json:"product"`
	Channel   string     `json:"channel"`
	Kind      string     `json:"kind"`
	Status    string     `json:"status"`
	Sender    string     `json:"sender"`
	Recipient string     `json:"recipient"`
	Subject   string     `json:"subject"`
	Body      string     `json:"body"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	SentAt    *time.Time `json:"sent_at,omitempty"`
}

type PersonReply struct {
	ID             int64     `json:"id"`
	Product        string    `json:"product,omitempty"`
	From           string    `json:"from"`
	Subject        string    `json:"subject"`
	Body           string    `json:"body"`
	Classification string    `json:"classification,omitempty"`
	ReceivedAt     time.Time `json:"received_at"`
}

// DoNotContact is why and since when nobody may message them.
type DoNotContact struct {
	Reason string    `json:"reason"`
	Detail string    `json:"detail,omitempty"`
	Since  time.Time `json:"since"`
}

// Person is everything known about one person, for the person page and the
// MCP tool person.
type Person struct {
	Person       *people.Person    `json:"person"`
	Identities   []people.Identity `json:"identities"`
	Facts        map[string]string `json:"facts"`
	Products     []string          `json:"products"`
	DoNotContact *DoNotContact     `json:"do_not_contact,omitempty"`
	EmailPolicy  []EmailPolicy     `json:"email_policy"`
	Scores       []PersonScore     `json:"scores"`
	Briefs       []Brief           `json:"briefs"`
	Enrollments  []Enrollment      `json:"enrollments"`
	Messages     []Message         `json:"messages"`
	Replies      []PersonReply     `json:"replies"`
	Signals      *signal.Feed      `json:"signals"`
	OpenTasks    []*outreach.Task  `json:"open_tasks"`
	// How they said they heard of you, per product, apart from first touch.
	ReportedSource []ReportedSource `json:"reported_source"`
	Conversations  []*notes.Note    `json:"conversations"`
}

// ReportedSource is a person's self-reported source for one product, next
// to the first-touch source Astrolabe recorded.
type ReportedSource struct {
	Product      string `json:"product"`
	SelfReported string `json:"self_reported"`
	FirstTouch   string `json:"first_touch"`
}

// FindPerson resolves an id or an email address to a person.
func FindPerson(ctx context.Context, q store.Q, who string) (*people.Person, error) {
	who = strings.TrimSpace(who)
	var id int64
	if _, err := fmt.Sscan(who, &id); err == nil && fmt.Sprint(id) == who {
		p, err := people.Get(ctx, q, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: no person %d", outreach.ErrInvalid, id)
		}
		return p, err
	}
	p, err := people.FindByEmail(ctx, q, who)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: no person with id or email %q", outreach.ErrInvalid, who)
	}
	return p, err
}

// GetPerson gathers everything about a person across the loaded products.
func GetPerson(ctx context.Context, out *outreach.Service, products []*product.Product, id int64, now time.Time) (*Person, error) {
	q := out.Pool
	p, err := people.Get(ctx, q, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: no person %d", outreach.ErrInvalid, id)
	} else if err != nil {
		return nil, err
	}
	v := &Person{Person: p, Scores: []PersonScore{}, EmailPolicy: []EmailPolicy{}}
	if v.Identities, err = people.Identities(ctx, q, id); err != nil {
		return nil, err
	}
	if v.Facts, err = people.Facts(ctx, q, id); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT product_id FROM product_people WHERE person_id = $1 ORDER BY product_id`, id)
	if err != nil {
		return nil, err
	}
	if v.Products, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, err
	}
	if v.Products == nil {
		v.Products = []string{}
	}
	var dnc DoNotContact
	if err := q.QueryRow(ctx, `SELECT reason, detail, created_at FROM suppressions WHERE person_id = $1`, id).Scan(&dnc.Reason, &dnc.Detail, &dnc.Since); err == nil {
		v.DoNotContact = &dnc
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	hasAddress := false
	for _, i := range v.Identities {
		hasAddress = hasAddress || i.Kind == "email"
	}
	for _, pr := range products {
		ok, reason, err := outreach.MayEmail(ctx, q, pr.ID, id)
		if err != nil {
			return nil, err
		}
		v.EmailPolicy = append(v.EmailPolicy, EmailPolicy{Product: pr.ID, MayEmail: ok, Reason: reason, HasAddress: hasAddress})
		if sc, found, err := score.Get(ctx, q, pr.ID, id); err != nil {
			return nil, err
		} else if found {
			v.Scores = append(v.Scores, PersonScore{sc, sc.Summary()})
		}
	}

	rows, err = q.Query(ctx, `SELECT DISTINCT ON (product_id) product_id, body, created_at FROM briefs WHERE person_id = $1 ORDER BY product_id, id DESC`, id)
	if err != nil {
		return nil, err
	}
	if v.Briefs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Brief, error) {
		var b Brief
		return b, r.Scan(&b.Product, &b.Body, &b.Written)
	}); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `
		SELECT e.id, e.product_id, e.sequence_id, e.status, e.stop_reason, e.next_step + 1, COALESCE(jsonb_array_length(s.config->'steps'), 0),
			e.next_due_at, e.started_at, e.play_id
		FROM enrollments e LEFT JOIN sequences s ON s.product_id = e.product_id AND s.id = e.sequence_id
		WHERE e.person_id = $1 ORDER BY e.id DESC`, id)
	if err != nil {
		return nil, err
	}
	if v.Enrollments, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Enrollment, error) {
		var e Enrollment
		return e, r.Scan(&e.ID, &e.Product, &e.Sequence, &e.Status, &e.StopReason, &e.NextStep, &e.Steps, &e.NextDue, &e.StartedAt, &e.Automation)
	}); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `
		SELECT id, product_id, channel, kind, status, sender, recipient, subject, body, error, created_at, sent_at
		FROM actions WHERE person_id = $1 ORDER BY id DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	if v.Messages, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Message, error) {
		var m Message
		return m, r.Scan(&m.ID, &m.Product, &m.Channel, &m.Kind, &m.Status, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &m.Error, &m.CreatedAt, &m.SentAt)
	}); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `
		SELECT id, COALESCE(product_id, ''), from_addr, subject, body, classification, received_at
		FROM replies WHERE person_id = $1 ORDER BY received_at DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	if v.Replies, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PersonReply, error) {
		var x PersonReply
		return x, r.Scan(&x.ID, &x.Product, &x.From, &x.Subject, &x.Body, &x.Classification, &x.ReceivedAt)
	}); err != nil {
		return nil, err
	}
	if v.Signals, err = signal.Recent(ctx, q, signal.Filter{PersonID: id, Days: 365, Limit: 50}, now); err != nil {
		return nil, err
	}
	tasks, err := out.Tasks(ctx, true, 1000)
	if err != nil {
		return nil, err
	}
	v.OpenTasks = []*outreach.Task{}
	for _, t := range tasks {
		if t.PersonID != nil && *t.PersonID == id {
			v.OpenTasks = append(v.OpenTasks, t)
		}
	}
	if v.Briefs == nil {
		v.Briefs = []Brief{}
	}
	if v.Enrollments == nil {
		v.Enrollments = []Enrollment{}
	}
	if v.Messages == nil {
		v.Messages = []Message{}
	}
	if v.Replies == nil {
		v.Replies = []PersonReply{}
	}
	rows, err = q.Query(ctx, `
		SELECT product_id, reported_source, source FROM product_people
		WHERE person_id = $1 AND reported_source <> '' ORDER BY product_id`, id)
	if err != nil {
		return nil, err
	}
	if v.ReportedSource, err = pgx.CollectRows(rows, pgx.RowToStructByPos[ReportedSource]); err != nil {
		return nil, err
	}
	if v.ReportedSource == nil {
		v.ReportedSource = []ReportedSource{}
	}
	if v.Conversations, err = notes.List(ctx, q, notes.ListOpts{PersonID: id}, now); err != nil {
		return nil, err
	}
	return v, nil
}

// SetFacts adds kind:value facts to a person by hand and rescores everyone
// (company facts count toward coworkers too). It returns the person's facts.
func SetFacts(ctx context.Context, pool *pgxpool.Pool, personID int64, facts []string) (map[string]string, error) {
	if err := people.SetFacts(ctx, pool, personID, facts, people.SourceManual); err != nil {
		return nil, fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	if _, err := score.RecomputeAll(ctx, pool, "", time.Now()); err != nil {
		return nil, err
	}
	return people.Facts(ctx, pool, personID)
}

// SplitFacts reads facts typed as "dep:bullmq, team:5-50" or one per line.
func SplitFacts(s string) []string {
	return strings.FieldsFunc(s, func(c rune) bool { return c == ',' || c == ';' || c == ' ' || c == '\n' || c == '\t' })
}

// StartSequence starts a sequence for people and drafts the first steps
// now rather than at the next minute. People who cannot be enrolled are in
// Skipped with the reason (already in a sequence, do-not-contact, or the
// outreach guard's refusal).
func StartSequence(ctx context.Context, out *outreach.Service, productID, sequenceID string, personIDs []int64) (outreach.EnrollResult, error) {
	if len(personIDs) == 0 {
		return outreach.EnrollResult{}, fmt.Errorf("%w: choose people first", outreach.ErrInvalid)
	}
	res, err := out.Enroll(ctx, productID, sequenceID, personIDs)
	if err != nil {
		return res, err
	}
	_, err = out.Tick(ctx)
	return res, err
}
