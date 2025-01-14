package oss

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	alioss "github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/go-kit/kit/log"
	"github.com/pkg/errors"
	"github.com/thanos-io/thanos/pkg/objstore"
	"gopkg.in/yaml.v2"
)

// Part size for multi part upload.
const PartSize = 1024 * 1024 * 128

// Config stores the configuration for oss bucket.
type Config struct {
	Endpoint        string `yaml:"endpoint"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	AccessKeySecret string `yaml:"access_key_secret"`
}

// Bucket implements the store.Bucket interface.
type Bucket struct {
	name   string
	logger log.Logger
	client *alioss.Client
	config Config
	bucket *alioss.Bucket
}

func NewTestBucket(t testing.TB) (objstore.Bucket, func(), error) {
	c := Config{
		Endpoint:        os.Getenv("ALIYUNOSS_ENDPOINT"),
		Bucket:          os.Getenv("ALIYUNOSS_BUCKET"),
		AccessKeyID:     os.Getenv("ALIYUNOSS_ACCESS_KEY_ID"),
		AccessKeySecret: os.Getenv("ALIYUNOSS_ACCESS_KEY_SECRET"),
	}

	if c.Endpoint == "" || c.AccessKeyID == "" || c.AccessKeySecret == "" {
		return nil, nil, errors.New("aliyun oss endpoint or access_key_id or access_key_secret " +
			"is not present in config file")
	}
	if c.Bucket != "" && os.Getenv("THANOS_ALLOW_EXISTING_BUCKET_USE") == "true" {
		t.Log("ALIYUNOSS_BUCKET is defined. Normally this tests will create temporary bucket " +
			"and delete it after test. Unset ALIYUNOSS_BUCKET env variable to use default logic. If you really want to run " +
			"tests against provided (NOT USED!) bucket, set THANOS_ALLOW_EXISTING_BUCKET_USE=true.")
		return NewTestBucketFromConfig(t, c, true)
	}
	return NewTestBucketFromConfig(t, c, false)
}

func calculateChunks(name string, r io.Reader) (int, int64, error) {
	switch r.(type) {
	case *os.File:
		f, _ := r.(*os.File)
		if fileInfo, err := f.Stat(); err == nil {
			s := fileInfo.Size()
			return int(math.Floor(float64(s) / PartSize)), s % PartSize, nil
		}
	case *strings.Reader:
		f, _ := r.(*strings.Reader)
		return int(math.Floor(float64(f.Size()) / PartSize)), f.Size() % PartSize, nil
	}
	return -1, 0, errors.New("unsupported implement of io.Reader")
}

// Upload the contents of the reader as an object into the bucket.
func (b *Bucket) Upload(ctx context.Context, name string, r io.Reader) error {
	chunksnum, lastslice, err := calculateChunks(name, r)
	if err != nil {
		return err
	}

	ncloser := ioutil.NopCloser(r)
	switch chunksnum {
	case 0:
		if err := b.bucket.PutObject(name, ncloser); err != nil {
			return errors.Wrap(err, "failed to upload oss object")
		}
	default:
		{
			init, err := b.bucket.InitiateMultipartUpload(name)
			if err != nil {
				return errors.Wrap(err, "failed to initiate multi-part upload")
			}
			chunk := 0
			uploadEveryPart := func(everypartsize int64, cnk int) (alioss.UploadPart, error) {
				prt, err := b.bucket.UploadPart(init, ncloser, everypartsize, cnk)
				if err != nil {
					if err := b.bucket.AbortMultipartUpload(init); err != nil {
						return prt, errors.Wrap(err, "failed to abort multi-part upload")
					}

					return prt, errors.Wrap(err, "failed to upload multi-part chunk")
				}
				return prt, nil
			}
			var parts []alioss.UploadPart
			for ; chunk < chunksnum; chunk++ {
				part, err := uploadEveryPart(PartSize, chunk+1)
				if err != nil {
					return errors.Wrap(err, "failed to upload every part")
				}
				parts = append(parts, part)
			}
			if lastslice != 0 {
				part, err := uploadEveryPart(lastslice, chunksnum+1)
				if err != nil {
					return errors.Wrap(err, "failed to upload the last chunk")
				}
				parts = append(parts, part)
			}
			if _, err := b.bucket.CompleteMultipartUpload(init, parts); err != nil {
				return errors.Wrap(err, "failed to set multi-part upload completive")
			}
		}
	}
	return nil
}

// Delete removes the object with the given name.
func (b *Bucket) Delete(ctx context.Context, name string) error {
	if err := b.bucket.DeleteObject(name); err != nil {
		return errors.Wrap(err, "delete oss object")
	}
	return nil
}

// NewBucket returns a new Bucket using the provided oss config values.
func NewBucket(logger log.Logger, conf []byte, component string) (*Bucket, error) {
	var config Config
	if err := yaml.Unmarshal(conf, &config); err != nil {
		return nil, errors.Wrap(err, "parse aliyun oss config file failed")
	}

	if config.Endpoint == "" || config.Bucket == "" || config.AccessKeyID == "" || config.AccessKeySecret == "" {
		return nil, errors.New("aliyun oss endpoint or bucket or access_key_id or access_key_secret " +
			"is not present in config file")
	}

	client, err := alioss.New(config.Endpoint, config.AccessKeyID, config.AccessKeySecret)
	if err != nil {
		return nil, errors.Wrap(err, "create aliyun oss client failed")
	}
	bk, err := client.Bucket(config.Bucket)
	if err != nil {
		return nil, errors.Wrapf(err, "use aliyun oss bucket %s failed", config.Bucket)
	}

	bkt := &Bucket{
		logger: logger,
		client: client,
		name:   config.Bucket,
		config: config,
		bucket: bk,
	}
	return bkt, nil
}

// Iter calls f for each entry in the given directory (not recursive). The argument to f is the full
// object name including the prefix of the inspected directory.
func (b *Bucket) Iter(ctx context.Context, dir string, f func(string) error) error {
	if dir != "" {
		dir = strings.TrimSuffix(dir, objstore.DirDelim) + objstore.DirDelim
	}

	marker := alioss.Marker("")
	for {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "context closed while iterating bucket")
		}
		objects, err := b.bucket.ListObjects(alioss.Prefix(dir), alioss.Delimiter(objstore.DirDelim), marker)
		if err != nil {
			return errors.Wrap(err, "listing aliyun oss bucket failed")
		}
		marker = alioss.Marker(objects.NextMarker)

		for _, object := range objects.Objects {
			if err := f(object.Key); err != nil {
				return errors.Wrapf(err, "callback func invoke for object %s failed ", object.Key)
			}
		}

		for _, object := range objects.CommonPrefixes {
			if err := f(object); err != nil {
				return errors.Wrapf(err, "callback func invoke for directory %s failed", object)
			}
		}
		if !objects.IsTruncated {
			break
		}
	}

	return nil
}

func (b *Bucket) Name() string {
	return b.name
}

func NewTestBucketFromConfig(t testing.TB, c Config, reuseBucket bool) (objstore.Bucket, func(), error) {
	if c.Bucket == "" {
		src := rand.NewSource(time.Now().UnixNano())

		bktToCreate := strings.Replace(fmt.Sprintf("test_%s_%x", strings.ToLower(t.Name()), src.Int63()), "_", "-", -1)
		if len(bktToCreate) >= 63 {
			bktToCreate = bktToCreate[:63]
		}
		testclient, err := alioss.New(c.Endpoint, c.AccessKeyID, c.AccessKeySecret)
		if err != nil {
			return nil, nil, errors.Wrap(err, "create aliyun oss client failed")
		}

		if err := testclient.CreateBucket(bktToCreate); err != nil {
			return nil, nil, errors.Wrapf(err, "create aliyun oss bucket %s failed", bktToCreate)
		}
		c.Bucket = bktToCreate
	}

	bc, err := yaml.Marshal(c)
	if err != nil {
		return nil, nil, err
	}

	b, err := NewBucket(log.NewNopLogger(), bc, "thanos-aliyun-oss-test")
	if err != nil {
		return nil, nil, err
	}

	if reuseBucket {
		if err := b.Iter(context.Background(), "", func(f string) error {
			return errors.Errorf("bucket %s is not empty", c.Bucket)
		}); err != nil {
			return nil, nil, errors.Wrapf(err, "oss check bucket %s", c.Bucket)
		}

		t.Log("WARNING. Reusing", c.Bucket, "Aliyun OSS bucket for OSS tests. Manual cleanup afterwards is required")
		return b, func() {}, nil
	}

	return b, func() {
		objstore.EmptyBucket(t, context.Background(), b)
		if err := b.client.DeleteBucket(c.Bucket); err != nil {
			t.Logf("deleting bucket %s failed: %s", c.Bucket, err)
		}
	}, nil
}

func (b *Bucket) Close() error { return nil }

func (b *Bucket) setRange(start, end int64, name string) (alioss.Option, error) {
	var opt alioss.Option
	if 0 <= start && start <= end {
		header, err := b.bucket.GetObjectMeta(name)
		if err != nil {
			return nil, err
		}

		size, err := strconv.ParseInt(header["Content-Length"][0], 10, 0)
		if err != nil {
			return nil, err
		}

		if end > size {
			end = size - 1
		}

		opt = alioss.Range(start, end)
	} else {
		return nil, errors.Errorf("Invalid range specified: start=%d end=%d", start, end)
	}
	return opt, nil
}

func (b *Bucket) getRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	if len(name) == 0 {
		return nil, errors.New("given object name should not empty")
	}

	var opts []alioss.Option
	if length != -1 {
		opt, err := b.setRange(off, off+length-1, name)
		if err != nil {
			return nil, err
		}
		opts = append(opts, opt)
	}

	resp, err := b.bucket.GetObject(name, opts...)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Get returns a reader for the given object name.
func (b *Bucket) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	return b.getRange(ctx, name, 0, -1)
}

func (b *Bucket) GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	return b.getRange(ctx, name, off, length)
}

// Exists checks if the given object exists in the bucket.
func (b *Bucket) Exists(ctx context.Context, name string) (bool, error) {
	exists, err := b.bucket.IsObjectExist(name)
	if err != nil {
		if b.IsObjNotFoundErr(err) {
			return false, nil
		}
		return false, errors.Wrap(err, "cloud not check if object exists")
	}

	return exists, nil
}

// IsObjNotFoundErr returns true if error means that object is not found. Relevant to Get operations.
func (b *Bucket) IsObjNotFoundErr(err error) bool {
	switch aliErr := err.(type) {
	case alioss.ServiceError:
		if aliErr.StatusCode == http.StatusNotFound {
			return true
		}
	}
	return false
}
