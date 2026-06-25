// Package state is the optional join table that recovers what externalId would have
// given us: a stable mapping from Google's immutable id to the Identity Store id.
//
//	users:  googleUserId  -> { icUserId, email }
//	groups: googleGroupId -> { icGroupId, displayName }
//
// With it, the engine matches by googleId first (surviving email / display-name
// changes as in-place renames instead of delete+recreate), falling back to natural-key
// matching when there's no mapping (first run, or state lost). It is therefore an
// optimization layer: losing the state file degrades to natural-key matching, it does
// not break correctness. Backed by S3 (enable bucket versioning); a no-op store keeps
// the stateless behavior when STATE_BUCKET is unset.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type UserRecord struct {
	ICUserID string `json:"ic_user_id"`
	Email    string `json:"email"`
}

type GroupRecord struct {
	ICGroupID   string `json:"ic_group_id"`
	DisplayName string `json:"display_name"`
}

// State is the persisted join table, keyed by Google's immutable ids.
type State struct {
	Users  map[string]UserRecord  `json:"users"`
	Groups map[string]GroupRecord `json:"groups"`
}

func New() State {
	return State{Users: map[string]UserRecord{}, Groups: map[string]GroupRecord{}}
}

// Store loads/saves the join table.
type Store interface {
	Load(ctx context.Context) (State, error)
	Save(ctx context.Context, s State) error
}

// Noop keeps the stateless (natural-key only) behavior.
type Noop struct{}

func (Noop) Load(context.Context) (State, error) { return New(), nil }
func (Noop) Save(context.Context, State) error   { return nil }

// S3API is the subset of the S3 client used (interface for testability).
type S3API interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type S3Store struct {
	api    S3API
	bucket string
	key    string
}

func NewS3Store(api S3API, bucket, key string) *S3Store {
	if key == "" {
		key = "state.json"
	}
	return &S3Store{api: api, bucket: bucket, key: key}
}

// Load reads and parses the state object. A missing object (first run) yields an empty
// state, not an error.
func (s *S3Store) Load(ctx context.Context) (State, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &s.key})
	if err != nil {
		var nsk *s3types.NoSuchKey
		var nf *s3types.NotFound
		if errors.As(err, &nsk) || errors.As(err, &nf) {
			return New(), nil
		}
		return New(), fmt.Errorf("get state s3://%s/%s: %w", s.bucket, s.key, err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return New(), fmt.Errorf("read state body: %w", err)
	}
	st := New()
	if len(b) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return New(), fmt.Errorf("parse state: %w", err)
	}
	if st.Users == nil {
		st.Users = map[string]UserRecord{}
	}
	if st.Groups == nil {
		st.Groups = map[string]GroupRecord{}
	}
	return st, nil
}

func (s *S3Store) Save(ctx context.Context, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	_, err = s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &s.key,
		Body:        bytes.NewReader(b),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("put state s3://%s/%s: %w", s.bucket, s.key, err)
	}
	return nil
}
