package p2p

import (
	"errors"

	core "github.com/libp2p/go-libp2p-core"
	"github.com/libp2p/go-libp2p-core/peer"
	libp2pCrypto "github.com/libp2p/go-libp2p-core/crypto"
	libp2pCryptoPb "github.com/libp2p/go-libp2p-core/crypto/pb"

	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
)

var (
	errCryptoNotSupported = errors.New("worker/common/p2p: crypto op not supported")

	libp2pContext = signature.NewContext("oasis-core/worker: libp2p")
)

type p2pSigner struct {
	signer signature.Signer
}

func (s *p2pSigner) Bytes() ([]byte, error) {
	return nil, errCryptoNotSupported
}

func (s *p2pSigner) Equals(other libp2pCrypto.Key) bool {
	return false
}

func (s *p2pSigner) Raw() ([]byte, error) {
	return nil, errCryptoNotSupported
}

func (s *p2pSigner) Type() libp2pCryptoPb.KeyType {
	return libp2pCryptoPb.KeyType_Ed25519
}

func (s *p2pSigner) Sign(msg []byte) ([]byte, error) {
	return s.signer.ContextSign(libp2pContext, msg)
}

func (s *p2pSigner) GetPublic() libp2pCrypto.PubKey {
	pubKey, err := PublicKeyToPubKey(s.signer.Public())
	if err != nil {
		panic(err)
	}

	return pubKey
}

// WrapSigner wraps an signature.Signer to be usable as a signer for the P2P subsystem.
func WrapSigner(signer signature.Signer) libp2pCrypto.PrivKey {
	return &p2pSigner{
		signer: signer,
	}
}

// PubKeyToPublicKey converts a libp2p PubKey into a signature.PublicKey.
func PubKeyToPublicKey(pubKey libp2pCrypto.PubKey) (signature.PublicKey, error) {
	var pk signature.PublicKey
	if pubKey.Type() != libp2pCrypto.Ed25519 {
		return pk, errCryptoNotSupported
	}

	raw, err := pubKey.Raw()
	if err != nil {
		return pk, err
	}

	if err = pk.UnmarshalBinary(raw); err != nil {
		return pk, err
	}

	return pk, nil
}

// PublicKeyToPubKey converts a signature.PublicKey into a libp2p PubKey.
func PublicKeyToPubKey(pk signature.PublicKey) (libp2pCrypto.PubKey, error) {
	return &libp2pPublicKey{
		inner: pk,
	}, nil
}

type libp2pPublicKey struct {
	inner signature.PublicKey
}

func (k *libp2pPublicKey) Bytes() ([]byte, error) {
	return libp2pCrypto.MarshalPublicKey(k)
}

func (k *libp2pPublicKey) Equals(other libp2pCrypto.Key) bool {
	otherK, ok := other.(*libp2pPublicKey)
	if !ok {
		return false
	}

	return k.inner.Equal(otherK.inner)
}

func (k *libp2pPublicKey) Raw() ([]byte, error) {
	return k.inner[:], nil
}

func (k *libp2pPublicKey) Type() libp2pCryptoPb.KeyType {
	return libp2pCryptoPb.KeyType_Ed25519
}

func (k *libp2pPublicKey) Verify(data, sig []byte) (bool, error) {
	return k.inner.Verify(libp2pContext, data, sig), nil
}

func unmarshalPublicKey(data []byte) (libp2pCrypto.PubKey, error) {
	var inner signature.PublicKey
	if err := inner.UnmarshalBinary(data); err != nil {
		return nil, err
	}

	return &libp2pPublicKey{
		inner: inner,
	}, nil
}

// PeerIDToPublicKey converts a libp2p2 PeerID into a signature.PublicKey.
func PeerIDToPublicKey(peerID core.PeerID) (signature.PublicKey, error) {
	pk, err := peerID.ExtractPublicKey()
	if err != nil {
		return signature.PublicKey{}, err
	}
	return PubKeyToPublicKey(pk)
}

// PublicKeyToPeerID converts a signature.PublicKey into a libp2p PeerID.
func PublicKeyToPeerID(pk signature.PublicKey) (core.PeerID, error) {
	pubKey, err := PublicKeyToPubKey(pk)
	if err != nil {
		return "", err
	}

	id, err := peer.IDFromPublicKey(pubKey)
	if err != nil {
		return "", err
	}

	return id, nil
}

func init() {
	libp2pCrypto.PubKeyUnmarshallers[libp2pCryptoPb.KeyType_Ed25519] = unmarshalPublicKey

	// There should be exactly 0 reasons why libp2p will ever need to
	// unmarshal a private key, as we explicitly pass in a signer.
	//
	// Ensure that it will fail.
	libp2pCrypto.PrivKeyUnmarshallers[libp2pCryptoPb.KeyType_Ed25519] = nil
}
