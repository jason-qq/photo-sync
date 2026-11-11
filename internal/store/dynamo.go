package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"photo-sync/internal/model"
)

// Dynamo is a Store backed by a single DynamoDB table (see DESIGN.md §5).
//
// Key layout, all items of a session in one partition, plus one global
// lookup item per reviewer link:
//
//	Session     PK=SESSION#{sid}  SK=META            GSI1PK=SESSIONS  GSI1SK={sid}
//	Photo       PK=SESSION#{sid}  SK=PHOTO#{pid}     LSI1SK=PHOTO#{filename}#{pid}
//	Hash lock   PK=SESSION#{sid}  SK=HASH#{hash}     (photoId)
//	Reviewer    PK=SESSION#{sid}  SK=REVIEWER#{rid}  (name, tokenHash, expiresAt, revoked)
//	Token       PK=TOKEN#{hash}   SK=META            (sessionId, reviewerId)
//	Feedback    PK=SESSION#{sid}  SK=FB#{rid}#{pid}  (verdict?, stars?, comment?, updatedAt)
//
// GSI1 lists sessions; LSI1 lists a session's photos in filename order.
type Dynamo struct {
	db    *dynamodb.Client
	table string
}

var _ Store = (*Dynamo)(nil)

// NewDynamo returns a Dynamo store using table, which must already exist
// with the layout created by CreateTable.
func NewDynamo(db *dynamodb.Client, table string) *Dynamo {
	return &Dynamo{db: db, table: table}
}

// Key attribute and index names.
//
// The key attributes are deliberately generic ("PK", not "sessionId")
// because in a single-table design one attribute holds different kinds of
// value depending on the item type: SK is "META" on a session, "PHOTO#…"
// on a photo, "HASH#…" on a hash lock. The type prefixes keep item kinds
// apart within a partition and let queries select one kind with
// begins_with.
//
// Index attributes are only written on the item types that belong in that
// index. DynamoDB leaves an item out of an index when it lacks the index's
// key attributes ("sparse index"), so each index holds just one kind of
// item.
//
// The index names are numbered rather than descriptive so a later access
// pattern can reuse an index with new prefixes, without a schema change.
const (
	// attrPK is the table's partition key: SESSION#{sid} on a session and
	// everything in it, so they live in one partition. Token lookup items
	// are the exception (TOKEN#{hash}): they must be found from the token
	// alone, before the session is known.
	attrPK = "PK"
	// attrSK is the table's sort key; identifies the item within its
	// session and encodes its type (see sessionPK etc. below).
	attrSK = "SK"

	// attrGSI1PK is GSI1's partition key. Set only on sessions, always to
	// gsi1PKAll, so that querying GSI1 for it returns every session.
	attrGSI1PK = "GSI1PK"
	// attrGSI1SK is GSI1's sort key: the session ID (a ULID), so sessions
	// come back in creation order.
	attrGSI1SK = "GSI1SK"
	// attrLSI1SK is LSI1's sort key (its partition key is PK). Set only on
	// photos, to PHOTO#{filename}#{pid}, so querying a session on LSI1
	// returns its photos in filename order. pid keeps the key unique when
	// two photos share a filename.
	attrLSI1SK = "LSI1SK"

	// indexGSI1 is a global secondary index on (GSI1PK, GSI1SK). Global:
	// its partition key differs from the table's, so it can span sessions.
	// Reads are eventually consistent only. Used to list sessions.
	indexGSI1 = "GSI1"
	// indexLSI1 is a local secondary index on (PK, LSI1SK). Local: same
	// partition key as the table, alternative sort order within one
	// session. Supports consistent reads, but can only be created with
	// the table. Used to list a session's photos by filename.
	indexLSI1 = "LSI1"

	// skSession is the SK of a session's own item. "META" is the usual
	// name for the item describing the partition itself.
	skSession = "META"
	// gsi1PKAll is the single GSI1 partition every session is written to.
	// One partition for all sessions is fine at personal scale; it would
	// need sharding at thousands of writes per second.
	gsi1PKAll = "SESSIONS"
	// photoPrefix starts both a photo's SK and its LSI1SK.
	photoPrefix = "PHOTO#"
)

// sessionPK is the PK of every item in session sid.
func sessionPK(sid string) string { return "SESSION#" + sid }

// photoSK is the SK of photo pid's item.
func photoSK(pid string) string { return photoPrefix + pid }

// hashSK is the SK of the lock item claiming a raw-file hash within a
// session. It exists so CreatePhoto can enforce one photo per hash: a
// conditional put on a known key is atomic, a "no photo with this hash"
// check would need a query, which can't be part of a transaction.
func hashSK(hash string) string { return "HASH#" + hash }

// photoLSI1SK is the LSI1 sort key of a photo.
func photoLSI1SK(filename, pid string) string { return photoPrefix + filename + "#" + pid }

// reviewerPrefix starts a reviewer's SK.
const reviewerPrefix = "REVIEWER#"

// reviewerSK is the SK of reviewer rid's item. The reviewer lives in the
// session's partition so that listing, extending and revoking use plain
// keys and consistent reads.
func reviewerSK(rid string) string { return reviewerPrefix + rid }

// feedbackPrefix starts a feedback item's SK. Feedback sorts by reviewer,
// then photo, so one reviewer's feedback is a single begins_with query.
const feedbackPrefix = "FB#"

// reviewerFeedbackPrefix is the SK prefix of all of reviewer rid's feedback.
func reviewerFeedbackPrefix(rid string) string { return feedbackPrefix + rid + "#" }

// feedbackSK is the SK of reviewer rid's feedback on photo pid.
func feedbackSK(rid, pid string) string { return reviewerFeedbackPrefix(rid) + pid }

// tokenPK is the PK of the lookup item for a link token's hash. It maps the
// token to its reviewer and never changes; expiry and revocation live only
// on the reviewer item.
func tokenPK(tokenHash string) string { return "TOKEN#" + tokenHash }

// Item shapes, one struct per item type, mapped to attributes by the
// dynamodbav tags. Key attributes are upper case (structural), data
// attributes camelCase (matching the JSON API). Timestamps are stored as
// RFC 3339 strings (the attributevalue default), readable in the console
// and sortable as text.

// sessionItem is a session: PK=SESSION#{sid}, SK=META, in GSI1.
type sessionItem struct {
	PK        string    `dynamodbav:"PK"`
	SK        string    `dynamodbav:"SK"`
	GSI1PK    string    `dynamodbav:"GSI1PK"`
	GSI1SK    string    `dynamodbav:"GSI1SK"`
	ID        string    `dynamodbav:"id"`
	Title     string    `dynamodbav:"title"`
	Date      string    `dynamodbav:"date"`
	CreatedAt time.Time `dynamodbav:"createdAt"`
}

// photoItem is a photo: PK=SESSION#{sid}, SK=PHOTO#{pid}, in LSI1.
type photoItem struct {
	PK        string            `dynamodbav:"PK"`
	SK        string            `dynamodbav:"SK"`
	LSI1SK    string            `dynamodbav:"LSI1SK"`
	ID        string            `dynamodbav:"id"`
	SessionID string            `dynamodbav:"sessionId"`
	Filename  string            `dynamodbav:"filename"`
	Hash      string            `dynamodbav:"hash"`
	Status    model.PhotoStatus `dynamodbav:"status"`
	CreatedAt time.Time         `dynamodbav:"createdAt"`
}

// hashItem is a hash lock: PK=SESSION#{sid}, SK=HASH#{hash}. It points at
// the photo holding the hash and is in no index.
type hashItem struct {
	PK      string `dynamodbav:"PK"`
	SK      string `dynamodbav:"SK"`
	PhotoID string `dynamodbav:"photoId"`
}

// reviewerItem is a reviewer: PK=SESSION#{sid}, SK=REVIEWER#{rid}.
type reviewerItem struct {
	PK        string    `dynamodbav:"PK"`
	SK        string    `dynamodbav:"SK"`
	ID        string    `dynamodbav:"id"`
	SessionID string    `dynamodbav:"sessionId"`
	Name      string    `dynamodbav:"name"`
	TokenHash string    `dynamodbav:"tokenHash"`
	ExpiresAt time.Time `dynamodbav:"expiresAt"`
	Revoked   bool      `dynamodbav:"revoked"`
	CreatedAt time.Time `dynamodbav:"createdAt"`
}

// tokenItem maps a link token's hash to its reviewer: PK=TOKEN#{hash},
// SK=META. In no index.
type tokenItem struct {
	PK         string `dynamodbav:"PK"`
	SK         string `dynamodbav:"SK"`
	SessionID  string `dynamodbav:"sessionId"`
	ReviewerID string `dynamodbav:"reviewerId"`
}

// feedbackItem is a reviewer's feedback on a photo: PK=SESSION#{sid},
// SK=FB#{rid}#{pid}. Unset fields are absent (omitempty), not stored as
// zero values.
type feedbackItem struct {
	PK         string        `dynamodbav:"PK"`
	SK         string        `dynamodbav:"SK"`
	SessionID  string        `dynamodbav:"sessionId"`
	ReviewerID string        `dynamodbav:"reviewerId"`
	PhotoID    string        `dynamodbav:"photoId"`
	Verdict    model.Verdict `dynamodbav:"verdict,omitempty"`
	Stars      int           `dynamodbav:"stars,omitempty"`
	Comment    string        `dynamodbav:"comment,omitempty"`
	UpdatedAt  time.Time     `dynamodbav:"updatedAt"`
}

func (it feedbackItem) model() model.Feedback {
	return model.Feedback{
		SessionID: it.SessionID, ReviewerID: it.ReviewerID, PhotoID: it.PhotoID,
		Verdict: it.Verdict, Stars: it.Stars, Comment: it.Comment, UpdatedAt: it.UpdatedAt,
	}
}

func (it reviewerItem) model() model.Reviewer {
	return model.Reviewer{
		ID: it.ID, SessionID: it.SessionID, Name: it.Name, TokenHash: it.TokenHash,
		ExpiresAt: it.ExpiresAt, Revoked: it.Revoked, CreatedAt: it.CreatedAt,
	}
}

func (it sessionItem) model() model.Session {
	return model.Session{ID: it.ID, Title: it.Title, Date: it.Date, CreatedAt: it.CreatedAt}
}

func (it photoItem) model() model.Photo {
	return model.Photo{
		ID: it.ID, SessionID: it.SessionID, Filename: it.Filename, Hash: it.Hash,
		Status: it.Status, CreatedAt: it.CreatedAt,
	}
}

func key(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		attrPK: &types.AttributeValueMemberS{Value: pk},
		attrSK: &types.AttributeValueMemberS{Value: sk},
	}
}

// notExists is the condition for creating an item only if its key is free.
var notExists = aws.String("attribute_not_exists(PK)")

func (d *Dynamo) CreateSession(ctx context.Context, s model.Session) error {
	item, err := attributevalue.MarshalMap(sessionItem{
		PK: sessionPK(s.ID), SK: skSession,
		GSI1PK: gsi1PKAll, GSI1SK: s.ID,
		ID: s.ID, Title: s.Title, Date: s.Date, CreatedAt: s.CreatedAt,
	})
	if err != nil {
		return err
	}
	_, err = d.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &d.table,
		Item:                item,
		ConditionExpression: notExists,
	})
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return ErrExists
	}
	return err
}

func (d *Dynamo) GetSession(ctx context.Context, id string) (model.Session, error) {
	var it sessionItem
	if err := d.get(ctx, sessionPK(id), skSession, &it); err != nil {
		return model.Session{}, err
	}
	return it.model(), nil
}

func (d *Dynamo) ListSessions(ctx context.Context) ([]model.Session, error) {
	// GSI1 is eventually consistent: a session created a moment ago may
	// be missing. Fine for a listing.
	items, err := queryAll[sessionItem](ctx, d.db, &dynamodb.QueryInput{
		TableName:              &d.table,
		IndexName:              aws.String(indexGSI1),
		KeyConditionExpression: aws.String("GSI1PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: gsi1PKAll},
		},
	})
	if err != nil {
		return nil, err
	}
	sessions := make([]model.Session, len(items))
	for i, it := range items {
		sessions[i] = it.model()
	}
	return sessions, nil
}

// CreatePhoto writes the photo and its hash lock in one transaction,
// conditional on the session existing and the hash being unclaimed. If the
// hash is taken, it returns the photo that holds it.
func (d *Dynamo) CreatePhoto(ctx context.Context, p model.Photo) (model.Photo, bool, error) {
	pk := sessionPK(p.SessionID)
	photo, err := attributevalue.MarshalMap(photoItem{
		PK: pk, SK: photoSK(p.ID), LSI1SK: photoLSI1SK(p.Filename, p.ID),
		ID: p.ID, SessionID: p.SessionID, Filename: p.Filename, Hash: p.Hash,
		Status: p.Status, CreatedAt: p.CreatedAt,
	})
	if err != nil {
		return model.Photo{}, false, err
	}
	lock, err := attributevalue.MarshalMap(hashItem{PK: pk, SK: hashSK(p.Hash), PhotoID: p.ID})
	if err != nil {
		return model.Photo{}, false, err
	}

	// Item order matters: CancellationReasons is indexed the same way.
	const (
		iSession = iota
		iHash
		iPhoto
	)
	in := &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			iSession: {ConditionCheck: &types.ConditionCheck{
				TableName:           &d.table,
				Key:                 key(pk, skSession),
				ConditionExpression: aws.String("attribute_exists(PK)"),
			}},
			iHash:  {Put: &types.Put{TableName: &d.table, Item: lock, ConditionExpression: notExists}},
			iPhoto: {Put: &types.Put{TableName: &d.table, Item: photo, ConditionExpression: notExists}},
		},
	}

	for attempt := 1; ; attempt++ {
		_, err = d.db.TransactWriteItems(ctx, in)
		if err == nil {
			return p, true, nil
		}
		var tce *types.TransactionCanceledException
		if !errors.As(err, &tce) {
			return model.Photo{}, false, err
		}
		reason := func(i int) string {
			if i >= len(tce.CancellationReasons) {
				return ""
			}
			return aws.ToString(tce.CancellationReasons[i].Code)
		}
		switch {
		case reason(iSession) == "ConditionalCheckFailed":
			return model.Photo{}, false, ErrNotFound
		case reason(iHash) == "ConditionalCheckFailed":
			existing, err := d.photoByHash(ctx, p.SessionID, p.Hash)
			return existing, false, err
		case reason(iPhoto) == "ConditionalCheckFailed":
			return model.Photo{}, false, ErrExists
		case conflicted(tce) && attempt < maxConflictRetries:
			// Lost a race with another transaction: one claiming the same
			// hash (the next attempt sees its outcome as a condition
			// failure), or any other photo upload to this session, since
			// every one checks the session item. Retry.
			if err := conflictBackoff(ctx, attempt); err != nil {
				return model.Photo{}, false, err
			}
			continue
		}
		return model.Photo{}, false, err
	}
}

// maxConflictRetries bounds transaction attempts that keep losing races
// with concurrent transactions on the same items.
const maxConflictRetries = 5

// conflicted reports whether a cancelled transaction lost a race with a
// concurrent transaction on one of its items. DynamoDB counts transactions
// that touch the same item as conflicting even when both only
// ConditionCheck it, so creates in one session (parallel photo uploads,
// which all check the session item) occasionally conflict.
func conflicted(tce *types.TransactionCanceledException) bool {
	for _, r := range tce.CancellationReasons {
		if aws.ToString(r.Code) == "TransactionConflict" {
			return true
		}
	}
	return false
}

// conflictBackoff waits before retry attempt+1: 20 ms × attempt plus up to
// 20 ms of jitter, so racing requests don't retry in lockstep.
func conflictBackoff(ctx context.Context, attempt int) error {
	return sleep(ctx, time.Duration(attempt)*20*time.Millisecond+rand.N(20*time.Millisecond))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Dynamo) photoByHash(ctx context.Context, sid, hash string) (model.Photo, error) {
	var lock hashItem
	if err := d.get(ctx, sessionPK(sid), hashSK(hash), &lock); err != nil {
		return model.Photo{}, fmt.Errorf("read hash lock: %w", err)
	}
	return d.GetPhoto(ctx, sid, lock.PhotoID)
}

func (d *Dynamo) GetPhoto(ctx context.Context, sessionID, photoID string) (model.Photo, error) {
	var it photoItem
	if err := d.get(ctx, sessionPK(sessionID), photoSK(photoID), &it); err != nil {
		return model.Photo{}, err
	}
	return it.model(), nil
}

func (d *Dynamo) ListPhotos(ctx context.Context, sessionID string) ([]model.Photo, error) {
	// Photos and the session are separate items; an empty photo list
	// can't tell a missing session from an empty one.
	if _, err := d.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	items, err := queryAll[photoItem](ctx, d.db, &dynamodb.QueryInput{
		TableName:              &d.table,
		IndexName:              aws.String(indexLSI1),
		ConsistentRead:         aws.Bool(true), // LSIs support it; GSIs don't
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(LSI1SK, :prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: sessionPK(sessionID)},
			":prefix": &types.AttributeValueMemberS{Value: photoPrefix},
		},
	})
	if err != nil {
		return nil, err
	}
	photos := make([]model.Photo, len(items))
	for i, it := range items {
		photos[i] = it.model()
	}
	return photos, nil
}

func (d *Dynamo) SetPhotoStatus(ctx context.Context, sessionID, photoID string, status model.PhotoStatus) error {
	return d.setAttr(ctx, sessionPK(sessionID), photoSK(photoID), "status", status)
}

// CreateReviewer writes the reviewer and its token lookup item in one
// transaction, conditional on the session existing and both keys being
// free.
func (d *Dynamo) CreateReviewer(ctx context.Context, r model.Reviewer) error {
	pk := sessionPK(r.SessionID)
	rev, err := attributevalue.MarshalMap(reviewerItem{
		PK: pk, SK: reviewerSK(r.ID),
		ID: r.ID, SessionID: r.SessionID, Name: r.Name, TokenHash: r.TokenHash,
		ExpiresAt: r.ExpiresAt, Revoked: r.Revoked, CreatedAt: r.CreatedAt,
	})
	if err != nil {
		return err
	}
	tok, err := attributevalue.MarshalMap(tokenItem{
		PK: tokenPK(r.TokenHash), SK: skSession, SessionID: r.SessionID, ReviewerID: r.ID,
	})
	if err != nil {
		return err
	}

	const (
		iSession = iota
		iToken
		iReviewer
	)
	in := &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			iSession: {ConditionCheck: &types.ConditionCheck{
				TableName:           &d.table,
				Key:                 key(pk, skSession),
				ConditionExpression: aws.String("attribute_exists(PK)"),
			}},
			iToken:    {Put: &types.Put{TableName: &d.table, Item: tok, ConditionExpression: notExists}},
			iReviewer: {Put: &types.Put{TableName: &d.table, Item: rev, ConditionExpression: notExists}},
		},
	}
	for attempt := 1; ; attempt++ {
		_, err = d.db.TransactWriteItems(ctx, in)
		var tce *types.TransactionCanceledException
		if !errors.As(err, &tce) {
			return err // nil on success
		}
		failed := func(i int) bool {
			return i < len(tce.CancellationReasons) &&
				aws.ToString(tce.CancellationReasons[i].Code) == "ConditionalCheckFailed"
		}
		switch {
		case failed(iSession):
			return ErrNotFound
		case failed(iToken), failed(iReviewer):
			return ErrExists
		case conflicted(tce) && attempt < maxConflictRetries:
			// Most likely a photo upload to the same session, which also
			// checks the session item. Retry.
			if err := conflictBackoff(ctx, attempt); err != nil {
				return err
			}
			continue
		}
		return err
	}
}

func (d *Dynamo) GetReviewer(ctx context.Context, sessionID, reviewerID string) (model.Reviewer, error) {
	var it reviewerItem
	if err := d.get(ctx, sessionPK(sessionID), reviewerSK(reviewerID), &it); err != nil {
		return model.Reviewer{}, err
	}
	return it.model(), nil
}

func (d *Dynamo) GetReviewerByTokenHash(ctx context.Context, tokenHash string) (model.Reviewer, error) {
	var tok tokenItem
	if err := d.get(ctx, tokenPK(tokenHash), skSession, &tok); err != nil {
		return model.Reviewer{}, err
	}
	return d.GetReviewer(ctx, tok.SessionID, tok.ReviewerID)
}

func (d *Dynamo) ListReviewers(ctx context.Context, sessionID string) ([]model.Reviewer, error) {
	if _, err := d.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	items, err := queryAll[reviewerItem](ctx, d.db, &dynamodb.QueryInput{
		TableName:              &d.table,
		ConsistentRead:         aws.Bool(true),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: sessionPK(sessionID)},
			":prefix": &types.AttributeValueMemberS{Value: reviewerPrefix},
		},
	})
	if err != nil {
		return nil, err
	}
	rs := make([]model.Reviewer, len(items))
	for i, it := range items {
		rs[i] = it.model()
	}
	return rs, nil
}

func (d *Dynamo) SetReviewerExpiry(ctx context.Context, sessionID, reviewerID string, expiresAt time.Time) error {
	return d.setAttr(ctx, sessionPK(sessionID), reviewerSK(reviewerID), "expiresAt", expiresAt)
}

func (d *Dynamo) RevokeReviewer(ctx context.Context, sessionID, reviewerID string) error {
	return d.setAttr(ctx, sessionPK(sessionID), reviewerSK(reviewerID), "revoked", true)
}

// UpdateFeedback applies the patch in a single UpdateItem: set fields go in
// SET, cleared ones in REMOVE (so unset fields are absent, as on create).
// DynamoDB applies one UpdateItem atomically, so concurrent patches of
// different fields both survive.
func (d *Dynamo) UpdateFeedback(ctx context.Context, sessionID, reviewerID, photoID string, patch model.FeedbackPatch, updatedAt time.Time) (model.Feedback, error) {
	names := map[string]string{}
	values := map[string]types.AttributeValue{}
	var set, remove []string

	// put adds "#name = :name" to SET, or "#name" to REMOVE if clear.
	put := func(name string, value any, clear bool) error {
		names["#"+name] = name // all names via placeholders; "comment" is reserved
		if clear {
			remove = append(remove, "#"+name)
			return nil
		}
		av, err := attributevalue.Marshal(value)
		if err != nil {
			return err
		}
		values[":"+name] = av
		set = append(set, "#"+name+" = :"+name)
		return nil
	}

	// Identity attributes are (re)written every time so a newly created
	// item is complete.
	for name, v := range map[string]any{
		"sessionId": sessionID, "reviewerId": reviewerID, "photoId": photoID, "updatedAt": updatedAt,
	} {
		if err := put(name, v, false); err != nil {
			return model.Feedback{}, err
		}
	}
	if p := patch.Verdict; p != nil {
		if err := put("verdict", *p, *p == model.VerdictNone); err != nil {
			return model.Feedback{}, err
		}
	}
	if p := patch.Stars; p != nil {
		if err := put("stars", *p, *p == 0); err != nil {
			return model.Feedback{}, err
		}
	}
	if p := patch.Comment; p != nil {
		if err := put("comment", *p, *p == ""); err != nil {
			return model.Feedback{}, err
		}
	}

	// Map iteration order is random; sort so expressions are stable
	// (easier to read in logs and errors).
	slices.Sort(set)
	expr := "SET " + strings.Join(set, ", ")
	if len(remove) > 0 {
		slices.Sort(remove)
		expr += " REMOVE " + strings.Join(remove, ", ")
	}

	out, err := d.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &d.table,
		Key:                       key(sessionPK(sessionID), feedbackSK(reviewerID, photoID)),
		UpdateExpression:          &expr,
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		return model.Feedback{}, err
	}
	var it feedbackItem
	if err := attributevalue.UnmarshalMap(out.Attributes, &it); err != nil {
		return model.Feedback{}, err
	}
	return it.model(), nil
}

func (d *Dynamo) DeleteFeedback(ctx context.Context, sessionID, reviewerID, photoID string) error {
	_, err := d.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &d.table,
		Key:       key(sessionPK(sessionID), feedbackSK(reviewerID, photoID)),
	})
	return err
}

func (d *Dynamo) ListFeedback(ctx context.Context, sessionID, reviewerID string) ([]model.Feedback, error) {
	return d.queryFeedback(ctx, sessionID, reviewerFeedbackPrefix(reviewerID))
}

func (d *Dynamo) ListSessionFeedback(ctx context.Context, sessionID string) ([]model.Feedback, error) {
	return d.queryFeedback(ctx, sessionID, feedbackPrefix)
}

// queryFeedback returns the session's feedback items whose SK starts with
// prefix, in SK order (reviewer, then photo).
func (d *Dynamo) queryFeedback(ctx context.Context, sessionID, prefix string) ([]model.Feedback, error) {
	items, err := queryAll[feedbackItem](ctx, d.db, &dynamodb.QueryInput{
		TableName:              &d.table,
		ConsistentRead:         aws.Bool(true),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: sessionPK(sessionID)},
			":prefix": &types.AttributeValueMemberS{Value: prefix},
		},
	})
	if err != nil {
		return nil, err
	}
	fs := make([]model.Feedback, len(items))
	for i, it := range items {
		fs[i] = it.model()
	}
	return fs, nil
}

// setAttr sets one attribute on an existing item, returning ErrNotFound if
// the item doesn't exist (instead of creating it, as UpdateItem would). The
// value is encoded like the item structs encode it.
func (d *Dynamo) setAttr(ctx context.Context, pk, sk, name string, value any) error {
	av, err := attributevalue.Marshal(value)
	if err != nil {
		return err
	}
	_, err = d.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &d.table,
		Key:                       key(pk, sk),
		UpdateExpression:          aws.String("SET #a = :v"),
		ConditionExpression:       aws.String("attribute_exists(PK)"),
		ExpressionAttributeNames:  map[string]string{"#a": name}, // names may be reserved words
		ExpressionAttributeValues: map[string]types.AttributeValue{":v": av},
	})
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return ErrNotFound
	}
	return err
}

// get reads one item with a strongly consistent read into out, returning
// ErrNotFound if it doesn't exist.
func (d *Dynamo) get(ctx context.Context, pk, sk string, out any) error {
	resp, err := d.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &d.table,
		Key:            key(pk, sk),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return err
	}
	if resp.Item == nil {
		return ErrNotFound
	}
	return attributevalue.UnmarshalMap(resp.Item, out)
}

// queryAll runs a query to completion, following pagination, and decodes
// every item as T. It always returns a non-nil slice on success.
func queryAll[T any](ctx context.Context, db *dynamodb.Client, in *dynamodb.QueryInput) ([]T, error) {
	out := []T{}
	pages := dynamodb.NewQueryPaginator(db, in)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := attributevalue.UnmarshalListOfMaps(page.Items, &items); err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// CreateTable creates table with the layout Dynamo expects (on-demand
// billing) and waits until it is active. It is a no-op if the table
// already exists. Intended for DynamoDB Local and tests; deployed tables
// are created by infrastructure code with the same definition.
func CreateTable(ctx context.Context, db *dynamodb.Client, table string) error {
	str := func(name string) types.AttributeDefinition {
		return types.AttributeDefinition{AttributeName: aws.String(name), AttributeType: types.ScalarAttributeTypeS}
	}
	keySchema := func(hash, rng string) []types.KeySchemaElement {
		return []types.KeySchemaElement{
			{AttributeName: aws.String(hash), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(rng), KeyType: types.KeyTypeRange},
		}
	}
	all := &types.Projection{ProjectionType: types.ProjectionTypeAll}

	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   &table,
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			str(attrPK), str(attrSK), str(attrGSI1PK), str(attrGSI1SK), str(attrLSI1SK),
		},
		KeySchema: keySchema(attrPK, attrSK),
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName: aws.String(indexGSI1), KeySchema: keySchema(attrGSI1PK, attrGSI1SK), Projection: all,
		}},
		// An LSI can only be defined when the table is created.
		LocalSecondaryIndexes: []types.LocalSecondaryIndex{{
			IndexName: aws.String(indexLSI1), KeySchema: keySchema(attrPK, attrLSI1SK), Projection: all,
		}},
	})
	var inUse *types.ResourceInUseException
	if errors.As(err, &inUse) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create table %s: %w", table, err)
	}
	return dynamodb.NewTableExistsWaiter(db).Wait(ctx, &dynamodb.DescribeTableInput{TableName: &table}, 2*time.Minute)
}
