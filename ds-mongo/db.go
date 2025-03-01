package dsmongo

import (
	"context"
	"time"

	dsq "github.com/ipfs/go-datastore/query"
	log "github.com/ipfs/go-log/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/xerrors"
)

var logging = log.Logger("dsrpc/dsmongo")

const (
	db_name         = "datastore"
	store_name      = "blocks"
	store_refs_name = "block_refs"
)

type Options struct {
	Uri           string
	DBName        string
	StoreName     string
	StoreRefsName string
}

func DefaultOptions() Options {
	return Options{
		Uri:           "mongodb://localhost:27017",
		DBName:        db_name,
		StoreName:     store_name,
		StoreRefsName: store_refs_name,
	}
}

type DSMongo struct {
	client *mongo.Client
	opts   Options
}

func NewDSMongo(opts Options) (*DSMongo, error) {
	defaultOpts := DefaultOptions()
	if opts.Uri == "" {
		opts.Uri = defaultOpts.Uri
	}
	if opts.DBName == "" {
		opts.DBName = defaultOpts.DBName
	}
	if opts.StoreName == "" {
		opts.StoreName = defaultOpts.StoreName
	}
	if opts.StoreRefsName == "" {
		opts.StoreRefsName = defaultOpts.StoreRefsName
	}
	var err error
	mgoClient, err := mongo.NewClient(options.Client().ApplyURI(opts.Uri))
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = mgoClient.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &DSMongo{
		client: mgoClient,
		opts:   opts,
	}, nil
}

type StoreItem struct {
	ID        string    `bson:"_id" json:"_id"`             // sha256 hash
	Value     []byte    `bson:"value" json:"value"`         // value
	RefCount  int       `bson:"ref_count" json:"ref_count"` // deprecated
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}

type RefItem struct {
	ID        string    `bson:"_id" json:"_id"` // key
	Ref       string    `bson:"ref" json:"ref"` // value
	Size      int64     `bson:"size" json:"size"`
	NID       []string  `bson:"nid" json:"nid"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}

// type refItemOnlySize struct {
// 	ID   string `bson:"_id" json:"_id"`
// 	Size int64  `bson:"size" json:"size"`
// }

type onlyRefCount struct {
	RefCount int `bson:"ref_count" json:"ref_count"`
}

func (dsm *DSMongo) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return dsm.client.Disconnect(ctx)
}

func (dsm *DSMongo) ds() *mongo.Collection {
	return dsm.client.Database(dsm.opts.DBName).Collection(dsm.opts.StoreName)
}
func (dsm *DSMongo) refs() *mongo.Collection {
	return dsm.client.Database(dsm.opts.DBName).Collection(dsm.opts.StoreRefsName)
}

func (dsm *DSMongo) Put(ctx context.Context, item *StoreItem, ref *RefItem) error {
	dstore := dsm.ds()
	refstore := dsm.refs()

	// 先看 refs 里是否存在记录
	hasref, _ := dsm.hasRef(ctx, ref.ID)

	// 再看 blocks 里是否有记录
	refCount := &onlyRefCount{}
	err := dstore.FindOne(ctx, bson.M{"_id": item.ID}).Decode(refCount)
	if err != nil && err != mongo.ErrNoDocuments {
		return err
	}
	if err == mongo.ErrNoDocuments { // 初次保存数据块
		item.RefCount = 1
		item.CreatedAt = time.Now()
		_, err := dstore.InsertOne(ctx, item)
		if err != nil {
			return err
		}
	}

	// 保存引用
	if !hasref {
		ref.Size = int64(len(item.Value))
		ref.CreatedAt = time.Now()
		ref.Ref = item.ID
		_, err = refstore.InsertOne(ctx, ref)
		if err != nil {
			return err
		}
	}

	//logging.Infof("mdb inserted id: %v", r.InsertedID)
	return nil
}

func (dsm *DSMongo) Delete(ctx context.Context, id string) error {
	dstore := dsm.ds()
	refstore := dsm.refs()

	var err error
	refItem := &RefItem{}

	err = refstore.FindOne(ctx, bson.M{"_id": id}).Decode(refItem)
	if err != nil {
		// if err == mongo.ErrNoDocuments {
		// 	return nil
		// }
		return err
	}

	// 删除 refstore 上的记录
	_, err = refstore.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}

	// 查看是否有其他针对数据块的引用
	rc, err := refstore.CountDocuments(ctx, bson.M{"ref": refItem.Ref})
	if err != nil {
		return err
	}

	if rc < 1 { // 不再被引用 删除数据
		_, err = dstore.DeleteOne(ctx, bson.M{"_id": refItem.Ref})
		if err != nil {
			return err
		}
		return nil
	}

	return nil
}

func (dsm *DSMongo) Get(ctx context.Context, id string) ([]byte, error) {
	dstore := dsm.ds()
	refstore := dsm.refs()

	ref := &RefItem{}
	err := refstore.FindOne(ctx, bson.M{"_id": id}).Decode(ref)
	if err != nil {
		return nil, err
	}
	b := &StoreItem{}
	err = dstore.FindOne(ctx, bson.M{"_id": ref.Ref}).Decode(b)
	if err != nil {
		return nil, err
	}

	return b.Value, nil
}

func (dsm *DSMongo) Has(ctx context.Context, id string) (bool, error) {
	return dsm.hasRef(ctx, id)
}

func (dsm *DSMongo) GetSize(ctx context.Context, id string) (int64, error) {
	refstore := dsm.refs()

	ref := &RefItem{}
	err := refstore.FindOne(ctx, bson.M{"_id": id}).Decode(&ref)
	if err != nil {
		return 0, err
	}

	return ref.Size, nil
}

func (dsm *DSMongo) Query(ctx context.Context, q dsq.Query) (chan *dsq.Entry, error) {
	if q.Orders != nil || q.Filters != nil {
		return nil, xerrors.Errorf("dsrpc currently not support orders or filters")
	}

	dstore := dsm.ds()
	refstore := dsm.refs()

	out := make(chan *dsq.Entry)
	closeChan := make(chan struct{})

	offset := int64(q.Offset)
	limit := int64(q.Limit)
	opts := options.FindOptions{}
	if offset > 0 {
		opts.Skip = &offset
	}
	if limit > 0 {
		opts.Limit = &limit
	}

	rge := primitive.Regex{Pattern: q.Prefix, Options: "i"}
	logging.Info(rge.String())
	logging.Info("rlock")
	cur, err := refstore.Find(ctx, bson.M{
		"_id": primitive.Regex{
			Pattern: "^" + q.Prefix,
			Options: "i",
		},
	}, &opts)
	logging.Info("un rlock")
	if err != nil {
		logging.Warn(err)
		return nil, err
	}
	logging.Info("get mongo cursor")

	//logging.Infof("cur next: %v", cur.Next(ctx))

	// refList := make([]*RefItem, 0)
	// for cur.Next(ctx) {
	// 	ref := &RefItem{}
	// 	err := cur.Decode(ref)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	logging.Infof("%v", *ref)
	// 	refList = append(refList, ref)
	// }

	go func(ctx context.Context, cur *mongo.Cursor, out chan *dsq.Entry, closeChan chan struct{}) {
		defer cur.Close(ctx)
		defer close(out)
		// if len(refList) == 0 {
		// 	close(out)
		// 	return
		// }
		// for _, ref := range refList {
		// 	ent := &dsq.Entry{
		// 		Key:  ref.ID,
		// 		Size: int(ref.Size),
		// 	}
		// 	out <- ent
		// }
		// close(out)

		for {
			select {
			case <-ctx.Done():
				return
			case <-closeChan:
				return
			default:
				if cur.Next(ctx) {
					ref := &RefItem{}
					err := cur.Decode(ref)
					if err != nil {
						return
					}
					ent := &dsq.Entry{
						Key:  ref.ID,
						Size: int(ref.Size),
					}
					if !q.KeysOnly {
						b := &StoreItem{}
						err = dstore.FindOne(ctx, bson.M{"_id": ref.Ref}).Decode(&b)
						if err != nil {
							return
						}
						ent.Value = b.Value
					}
					logging.Infof("key: %v, size: %v", ent.Key, ent.Size)
					out <- ent
				} else {
					return
				}
			}
		}

	}(ctx, cur, out, closeChan)

	return out, nil
}

func (dsm *DSMongo) hasRef(ctx context.Context, id string) (bool, error) {
	refstore := dsm.refs()

	err := refstore.FindOne(ctx, bson.M{"_id": id}).Err()

	if err != nil {
		return false, err
	}

	return true, nil
}

// func (dsm *DSMongo) hasBlock(ctx context.Context, id string) (bool, error) {
// 	dstore := dsm.ds()

// 	err := dstore.FindOne(ctx, bson.M{"_id": id}).Err()

// 	if err != nil {
// 		return false, err
// 	}

// 	return true, nil
// }
