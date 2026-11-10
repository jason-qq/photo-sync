package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"photo-sync/internal/model"
)

// Dynamo is a Store backed by a single DynamoDB table (see DESIGN.md §5).
//
// Key layout, all items of a session in one partition:
//
//	Session     PK=SESSION#{sid}  SK=META           GSI1PK=SESSIONS  GSI1SK={sid}
//	Photo       PK=SESSION#{sid}  SK=PHOTO#{pid}    LSI1SK=PHOTO#{filename}#{pid}
//	Hash lock   PK=SESSION#{sid}  SK=HASH#{hash}    (photoId)
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
	// attrPK is the table's partition key: SESSION#{sid} on every item,
	// so a session and everything in it live in one partition.
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
		case reason(iHash) == "TransactionConflict" && attempt < maxConflictRetries:
			// A concurrent transaction is claiming the same hash. Retry;
			// the next attempt will see its outcome as a condition failure.
			if err := sleep(ctx, time.Duration(attempt)*20*time.Millisecond); err != nil {
				return model.Photo{}, false, err
			}
			continue
		}
		return model.Photo{}, false, err
	}
}

// maxConflictRetries bounds CreatePhoto attempts when concurrent uploads of
// the same file collide.
const maxConflictRetries = 5

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
	_, err := d.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                &d.table,
		Key:                      key(sessionPK(sessionID), photoSK(photoID)),
		UpdateExpression:         aws.String("SET #status = :status"),
		ConditionExpression:      aws.String("attribute_exists(PK)"),
		ExpressionAttributeNames: map[string]string{"#status": "status"}, // reserved word
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":status": &types.AttributeValueMemberS{Value: string(status)},
		},
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
