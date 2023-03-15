package contest

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/x/mongo/driver/connstring"
)

var (
	mongodbColLogEntry = "log"
	mongodbIndexPrefix = "contest_log"
)

var logEntryIndexModel = []mongo.IndexModel{
	{
		Keys:    bson.D{bson.E{Key: "node", Value: 1}},
		Options: options.Index().SetName(mongodbIndexPrefix + "_node"),
	},
	{
		Keys:    bson.D{bson.E{Key: "error", Value: 1}},
		Options: options.Index().SetName(mongodbIndexPrefix + "_error"),
	},
}

type Mongodb struct {
	client *mongo.Client
	db     *mongo.Database
}

func NewMongodbFromURI(ctx context.Context, uri string) (*Mongodb, error) {
	e := util.StringErrorFunc("connect mongodb")

	cs, err := connstring.Parse(uri)
	if err != nil {
		return nil, e(err, "")
	}

	if len(cs.Database) < 1 {
		return nil, e(err, "empty database")
	}

	db := &Mongodb{}

	if err := db.connect(ctx, cs); err != nil {
		return nil, e(err, "")
	}

	return db, nil
}

func (db *Mongodb) connect(ctx context.Context, cs connstring.ConnString) error {
	clientOpts := options.Client().ApplyURI(cs.String())
	if err := clientOpts.Validate(); err != nil {
		return errors.WithStack(err)
	}

	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		return errors.WithStack(err)
	}

	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		return errors.WithStack(err)
	}

	db.client = client
	db.db = client.Database(cs.Database)

	return db.createIndices(ctx, mongodbColLogEntry, logEntryIndexModel, mongodbColLogEntry)
}

func (db *Mongodb) Close(ctx context.Context) error {
	return errors.WithStack(db.client.Disconnect(ctx))
}

func (db *Mongodb) InsertLogEntries(ctx context.Context, entries []LogEntry) error {
	if db.client == nil || db.db == nil {
		return errors.Errorf("not yet connected")
	}

	models := make([]mongo.WriteModel, len(entries))
	for i := range entries {
		models[i] = mongo.NewInsertOneModel().SetDocument(entries[i])
	}

	opts := options.BulkWrite().SetOrdered(true)
	if _, err := db.db.Collection(mongodbColLogEntry).BulkWrite(ctx, models, opts); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (db *Mongodb) Find(ctx context.Context, query bson.M) (record map[string]interface{}, found bool, _ error) {
	option := options.FindOne()
	option = option.SetSort(bson.D{{Key: "_id", Value: -1}})

	if r := db.db.Collection(mongodbColLogEntry).FindOne(ctx, query, option); r.Err() != nil {
		if errors.Is(r.Err(), mongo.ErrNoDocuments) {
			return nil, false, nil
		}

		return nil, false, r.Err()
	} else if err := r.Decode(&record); err != nil {
		return nil, true, errors.WithStack(err)
	} else {
		return record, true, nil
	}
}

func (db *Mongodb) createIndices(ctx context.Context, col string, models []mongo.IndexModel, prefix string) error {
	iv := db.db.Collection(col).Indexes()

	cursor, err := iv.List(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	var results []bson.M
	if err = cursor.All(ctx, &results); err != nil {
		return errors.WithStack(err)
	}

	var existings []string //nolint:prealloc //...

	for _, r := range results {
		name := r["name"].(string) //nolint:forcetypeassert //...
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		existings = append(existings, name)
	}

	if len(existings) > 0 {
		for _, name := range existings {
			if _, err := iv.DropOne(ctx, name); err != nil {
				return errors.WithStack(err)
			}
		}
	}

	if len(models) < 1 {
		return nil
	}

	if _, err := iv.CreateMany(ctx, models); err != nil {
		return errors.WithStack(err)
	}

	return nil
}
