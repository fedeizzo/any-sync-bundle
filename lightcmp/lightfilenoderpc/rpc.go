package lightfilenoderpc

import (
	"context"
	"fmt"

	"github.com/anyproto/any-sync/acl"
	"github.com/anyproto/any-sync/app"
	"github.com/anyproto/any-sync/app/logger"
	"github.com/anyproto/any-sync/commonfile/fileproto"
	"github.com/anyproto/any-sync/commonfile/fileproto/fileprotoerr"
	"github.com/anyproto/any-sync/net/rpc/server"
	"github.com/dgraph-io/badger/v4"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"go.uber.org/zap"

	"github.com/grishy/any-sync-bundle/lightcmp/lightfilenodestore"
)

const (
	CName        = "light.filenode.rpc"
	cidSizeLimit = 2 << 20 // 2 Mb
)

var log = logger.NewNamed(CName)

type lightFileNodeRpc struct {
	acl   acl.AclService
	dRPC  server.DRPCServer
	store lightfilenodestore.StoreService
}

func New() Service {
	return new(lightFileNodeRpc)
}

type Service interface {
	app.Component
}

//
// App Component
//

func (r *lightFileNodeRpc) Init(a *app.App) error {
	log.Info("call Init")

	r.dRPC = app.MustComponent[server.DRPCServer](a)
	r.acl = app.MustComponent[acl.AclService](a)
	r.store = app.MustComponent[lightfilenodestore.StoreService](a)

	return nil
}

func (r *lightFileNodeRpc) Name() (name string) {
	return CName
}

//
// App Component Runnable
//

func (r *lightFileNodeRpc) Run(ctx context.Context) error {
	return fileproto.DRPCRegisterFile(r.dRPC, r)
}

func (r *lightFileNodeRpc) Close(ctx context.Context) error {
	return nil
}

//
// Component methods
//

func (r *lightFileNodeRpc) BlockGet(ctx context.Context, req *fileproto.BlockGetRequest) (*fileproto.BlockGetResponse, error) {
	log.InfoCtx(ctx, "BlockGet",
		zap.String("spaceId", req.SpaceId),
		zap.Strings("cid", cidsToStrings(req.Cid)),
		zap.Bool("wait", req.Wait),
	)

	c, err := cid.Cast(req.Cid)
	if err != nil {
		return nil, fmt.Errorf("failed to cast CID: %w", err)
	}

	var blockObj *lightfilenodestore.BlockObj

	errTx := r.store.TxView(func(txn *badger.Txn) error {
		var errGet error
		blockObj, errGet = r.store.GetBlock(txn, c, req.SpaceId, req.Wait)
		return errGet
	})
	if errTx != nil {
		return nil, fmt.Errorf("transaction view failed: %w", errTx)
	}

	resp := &fileproto.BlockGetResponse{
		Cid:  req.Cid,
		Data: blockObj.Data(),
	}

	return resp, nil
}

func (r *lightFileNodeRpc) BlockPush(ctx context.Context, req *fileproto.BlockPushRequest) (*fileproto.Ok, error) {
	log.InfoCtx(ctx, "BlockPush",
		zap.String("spaceId", req.SpaceId),
		zap.String("fileId", req.FileId),
		zap.Strings("cid", cidsToStrings(req.Cid)),
		zap.Int("dataSize", len(req.Data)),
	)

	// Verify that we have access to store and enough space
	if ok, err := r.canWrite(ctx, req.SpaceId); err != nil {
		return nil, fmt.Errorf("failed to check write access: %w", err)
	} else if !ok {
		return nil, err
	}

	// Check block itself
	c, err := cid.Cast(req.Cid)
	if err != nil {
		return nil, fmt.Errorf("failed to cast CID: %w", err)
	}

	b, err := blocks.NewBlockWithCid(req.Data, c)
	if err != nil {
		return nil, fmt.Errorf("failed to create block: %w", err)
	}

	if len(req.Data) > cidSizeLimit {
		return nil, fileprotoerr.ErrQuerySizeExceeded
	}

	chkc, err := c.Prefix().Sum(req.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate block data checksum: %w", err)
	}

	if !chkc.Equals(c) {
		return nil, fmt.Errorf("block data checksum mismatch: %s != %s", chkc, c)
	}

	// Finally, save the block and update all necessary counters
	errTx := r.store.TxUpdate(func(txn *badger.Txn) error {
		if err := r.store.PushBlock(txn, req.SpaceId, b); err != nil {
			return fmt.Errorf("failed to push block: %w", err)
		}

		// And connect the block to the file
		// TODO: Implement logic to connect the block to the file in the datastore

		return nil
	})

	if errTx != nil {
		return nil, fmt.Errorf("transaction view failed: %w", errTx)
	}

	return &fileproto.Ok{}, errTx
}

func (r *lightFileNodeRpc) BlocksCheck(ctx context.Context, request *fileproto.BlocksCheckRequest) (*fileproto.BlocksCheckResponse, error) {
	log.InfoCtx(ctx, "BlocksCheck",
		zap.String("spaceId", request.SpaceId),
		zap.Strings("cids", cidsToStrings(request.Cids...)),
	)

	return &fileproto.BlocksCheckResponse{
		BlocksAvailability: []*fileproto.BlockAvailability{
			{
				Cid:    request.Cids[0],
				Status: fileproto.AvailabilityStatus_NotExists,
			},
		},
	}, nil
}

func (r *lightFileNodeRpc) BlocksBind(ctx context.Context, request *fileproto.BlocksBindRequest) (*fileproto.Ok, error) {
	log.InfoCtx(ctx, "BlocksBind",
		zap.String("spaceId", request.SpaceId),
		zap.String("fileId", request.FileId),
		zap.Strings("cids", cidsToStrings(request.Cids...)),
	)

	return &fileproto.Ok{}, nil
}

func (r *lightFileNodeRpc) FilesDelete(ctx context.Context, request *fileproto.FilesDeleteRequest) (*fileproto.FilesDeleteResponse, error) {
	log.InfoCtx(ctx, "FilesDelete",
		zap.String("spaceId", request.SpaceId),
		zap.Strings("fileIds", request.FileIds),
	)

	// r.badgerDB.NewWriteBatch()

	// err := r.badgerDB.Update(func(txn *badger.Txn) error {
	// 	// For each file, decrement reference counter and delete the key
	// 	prefix := []byte(fmt.Sprintf("l:%s.%s:", request.SpaceId, request.FileIds[0]))
	// 	opts := badger.IteratorOptions{PrefetchSize: 1000}

	// 	it := txn.NewIterator(opts)
	// 	defer it.Close()

	// 	batch := txn.NewBatch()
	// 	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
	// 		cid := extractCID(it.Item().Key())

	// 		batch.Merge(rcKey(cid), uint64ToBytes(^uint64(0))) // Decrement
	// 		batch.Delete(it.Item().Key())

	// 		if batch.Len() >= 1000 {
	// 			if err := batch.Flush(); err != nil {
	// 				return err
	// 			}
	// 			batch = txn.NewBatch()
	// 		}
	// 	}
	// 	return batch.Flush()
	// })

	return &fileproto.FilesDeleteResponse{}, nil
}

func (r *lightFileNodeRpc) FilesInfo(ctx context.Context, request *fileproto.FilesInfoRequest) (*fileproto.FilesInfoResponse, error) {
	log.InfoCtx(ctx, "FilesInfo",
		zap.String("spaceId", request.SpaceId),
		zap.Strings("fileIds", request.FileIds),
	)

	// Store usage and CID count in the same key
	// Because we always update both values together
	// Key format: f:<spaceId>.<fileId> -> [<usageBytes> uint64][<cidsCount> uint32}

	return &fileproto.FilesInfoResponse{
		FilesInfo: []*fileproto.FileInfo{
			{
				FileId:     request.FileIds[0],
				UsageBytes: 123,
				CidsCount:  100,
			},
		},
	}, nil
}

func (r *lightFileNodeRpc) FilesGet(request *fileproto.FilesGetRequest, stream fileproto.DRPCFile_FilesGetStream) error {
	log.InfoCtx(context.Background(), "FilesGet",
		zap.String("spaceId", request.SpaceId),
	)
	for i := 0; i < 10; i++ {
		err := stream.Send(&fileproto.FilesGetResponse{
			FileId: "test",
		})
		if err != nil {
			log.ErrorCtx(context.Background(), "FilesGet stream send failed",
				zap.Error(err))
			return err
		}
	}

	return nil
}

func (r *lightFileNodeRpc) Check(ctx context.Context, request *fileproto.CheckRequest) (*fileproto.CheckResponse, error) {
	log.InfoCtx(ctx, "Check")

	return &fileproto.CheckResponse{
		SpaceIds:   []string{"space1", "space2"},
		AllowWrite: true,
	}, nil
}

func (r *lightFileNodeRpc) SpaceInfo(ctx context.Context, request *fileproto.SpaceInfoRequest) (*fileproto.SpaceInfoResponse, error) {
	log.InfoCtx(ctx, "SpaceInfo",
		zap.String("spaceId", request.SpaceId),
	)

	// Use file approach for counter that updated on each block push

	return &fileproto.SpaceInfoResponse{
		LimitBytes:      100,
		TotalUsageBytes: 40,
		CidsCount:       10,
		FilesCount:      2,
		SpaceUsageBytes: 100,
		SpaceId:         request.SpaceId,
	}, nil
}

func (r *lightFileNodeRpc) AccountInfo(ctx context.Context, request *fileproto.AccountInfoRequest) (*fileproto.AccountInfoResponse, error) {
	log.InfoCtx(ctx, "AccountInfo")

	// Use file approach for counter that updated on each block push

	return &fileproto.AccountInfoResponse{
		LimitBytes:      100,
		TotalUsageBytes: 30,
		TotalCidsCount:  100,
		Spaces: []*fileproto.SpaceInfoResponse{
			{
				LimitBytes:      100,
				TotalUsageBytes: 222,
				CidsCount:       34,
				FilesCount:      123,
				SpaceUsageBytes: 555,
				SpaceId:         "space1",
			},
		},
		AccountLimitBytes: 29,
	}, nil
}

func (r *lightFileNodeRpc) AccountLimitSet(ctx context.Context, request *fileproto.AccountLimitSetRequest) (*fileproto.Ok, error) {
	log.InfoCtx(ctx, "AccountLimitSet",
		zap.String("identity", request.Identity),
		zap.Uint64("limit", request.Limit),
	)

	return &fileproto.Ok{}, nil
}

func (r *lightFileNodeRpc) SpaceLimitSet(ctx context.Context, request *fileproto.SpaceLimitSetRequest) (*fileproto.Ok, error) {
	log.InfoCtx(ctx, "SpaceLimitSet",
		zap.String("spaceId", request.SpaceId),
		zap.Uint64("limit", request.Limit),
	)

	return &fileproto.Ok{}, nil
}
