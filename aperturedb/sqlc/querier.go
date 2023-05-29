// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.18.0

package sqlc

import (
	"context"
)

type Querier interface {
	DeleteOnionPrivateKey(ctx context.Context) error
	DeleteSecretByHash(ctx context.Context, hash []byte) (int64, error)
	GetSecretByHash(ctx context.Context, hash []byte) ([]byte, error)
	GetSession(ctx context.Context, passphraseEntropy []byte) (LncSession, error)
	InsertSecret(ctx context.Context, arg InsertSecretParams) (int32, error)
	InsertSession(ctx context.Context, arg InsertSessionParams) error
	SelectOnionPrivateKey(ctx context.Context) ([]byte, error)
	SetExpiry(ctx context.Context, arg SetExpiryParams) error
	SetRemotePubKey(ctx context.Context, arg SetRemotePubKeyParams) error
	UpsertOnion(ctx context.Context, arg UpsertOnionParams) error
}

var _ Querier = (*Queries)(nil)
