package hyperliquid

import (
	"context"
	"crypto/ecdsa"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

type Account interface {
	Address() common.Address
	SignTypedData(ctx context.Context, typedData apitypes.TypedData) (*SignatureResult, error)
}

type accountPrimitive struct {
	privateKey *ecdsa.PrivateKey
}

// NewAccount creates a new Account from an ECDSA private key.
func NewAccount(privateKey *ecdsa.PrivateKey) Account {
	return &accountPrimitive{privateKey: privateKey}
}

func (a *accountPrimitive) Address() common.Address {
	return crypto.PubkeyToAddress(a.privateKey.PublicKey)
}

func (a *accountPrimitive) SignTypedData(_ context.Context, typedData apitypes.TypedData) (*SignatureResult, error) {
	result, err := signInner(a.privateKey, typedData)
	if err != nil {
		return nil, err
	}
	return &result, nil
}
