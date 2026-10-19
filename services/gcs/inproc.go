package gcs

import (
	"context"
	"io"

	storage "google.golang.org/api/storage/v1"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

var _ emu.ObjectStore = (*Service)(nil)

// appendAttempts bounds how often AppendObject retries when another write
// of the object came between its read and its commit.
const appendAttempts = 5

// running refuses in-process calls while the Service is not started, as
// at the Instance's start when another Service starts first.
func (s *Service) running() error {
	if !s.ready.Load() {
		return apierr.New(codes.Unavailable, "Cloud Storage is not running.")
	}
	return nil
}

// ReadObject implements emu.ObjectStore.
func (s *Service) ReadObject(ctx context.Context, bucket, name string, gen int64) (io.ReadCloser, int64, error) {
	if err := s.running(); err != nil {
		return nil, 0, err
	}
	rec, _, err := s.readObject(bucket, name, gen, conds{})
	if err != nil {
		return nil, 0, err
	}
	f, err := s.blobs.open(rec.Blob)
	if err != nil {
		return nil, 0, apierr.Internal("object data unavailable: %v", err)
	}
	return f, rec.Object.Generation, nil
}

// AppendObject implements emu.ObjectStore. Blobs are never modified in
// place, so each call copies the object's data into a new blob with data
// after it, and commits it as a new generation on the condition that the
// one it copied is still live.
func (s *Service) AppendObject(ctx context.Context, bucket, name, contentType string, data []byte) error {
	if err := s.running(); err != nil {
		return err
	}
	ctx = system(ctx)
	if !validObjectName(name) {
		return errInvalid("The specified object name is not valid.")
	}
	for attempt := 1; ; attempt++ {
		err := s.appendOnce(ctx, bucket, name, contentType, data)
		if e := apierr.From(err); e == nil || e.Code != codes.FailedPrecondition || attempt == appendAttempts {
			return err
		}
	}
}

func (s *Service) appendOnce(ctx context.Context, bucket, name, contentType string, data []byte) error {
	var cur *objectRec
	err := s.env.Store.View(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		var err error
		cur, _, err = loadObject(tx, bucket, name, 0)
		return err
	})
	if err != nil {
		return err
	}
	meta := &storage.Object{Name: name, ContentType: contentType}
	var gen int64
	bw, err := s.blobs.newWriter()
	if err != nil {
		return err
	}
	if cur != nil {
		o := cur.Object
		gen = o.Generation
		meta.ContentType, meta.CacheControl, meta.ContentEncoding, meta.Metadata = o.ContentType, o.CacheControl, o.ContentEncoding, o.Metadata
		if err := s.copyBlob(bw, cur.Blob); err != nil {
			bw.abort()
			return err
		}
	}
	if _, err := bw.Write(data); err != nil {
		bw.abort()
		return err
	}
	info, err := bw.commit()
	if err != nil {
		return err
	}
	_, _, err = s.commitObject(ctx, &writeReq{bucket: bucket, meta: meta, blob: info, conds: conds{GenMatch: &gen}})
	return err
}

// copyBlob copies blob id into w.
func (s *Service) copyBlob(w *blobWriter, id string) error {
	f, err := s.blobs.open(id)
	if err != nil {
		return apierr.Internal("object data unavailable: %v", err)
	}
	defer f.Close()
	_, err = w.ReadFrom(f)
	return err
}

// EnsureServiceBucket implements emu.ObjectStore.
func (s *Service) EnsureServiceBucket(ctx context.Context, project, name string) error {
	if err := s.running(); err != nil {
		return err
	}
	if _, err := s.getBucketRec(name); err == nil {
		return nil
	} else if e := apierr.From(err); e.Code != codes.NotFound {
		return err
	}
	_, err := s.storeBucket(system(ctx), project, &storage.Bucket{Name: name}, false, nil, true)
	if e := apierr.From(err); e != nil && e.Code == codes.AlreadyExists {
		return nil
	}
	return err
}
