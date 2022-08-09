// Package dilithium_ecdsa implements hybrid Dilithium + ECDSA encryption, suitable for OpenPGP, experimental.
package dilithium_ecdsa

import (
	"crypto/subtle"
	goerrors "errors"
	"io"
	"math/big"

	"github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/internal/ecc"
	dilithium "github.com/kudelskisecurity/crystals-go/crystals-dilithium"
	"golang.org/x/crypto/sha3"
)

type PublicKey struct {
	AlgId uint8
	Curve ecc.ECDSACurve
	Dilithium *dilithium.Dilithium
	X, Y *big.Int
	PublicDilithium []byte
}

type PrivateKey struct {
	PublicKey
	SecretEC *big.Int
	SecretDilithium []byte
}

func NewPublicKey(curve ecc.ECDSACurve, dilithium *dilithium.Dilithium) *PublicKey {
	return &PublicKey{
		Curve: curve,
		Dilithium: dilithium,
	}
}

func NewPrivateKey(key PublicKey) *PrivateKey {
	return &PrivateKey{
		PublicKey: key,
	}
}


func (pk *PublicKey) MarshalPoint() []byte {
	return pk.Curve.MarshalIntegerPoint(pk.X, pk.Y)
}

func (pk *PublicKey) UnmarshalPoint(p []byte) error {
	pk.X, pk.Y = pk.Curve.UnmarshalIntegerPoint(p)
	if pk.X == nil {
		return goerrors.New("dilithium_ecdsa: failed to parse EC point")
	}
	return nil
}

func (sk *PrivateKey) MarshalIntegerSecret() []byte {
	return sk.Curve.MarshalFieldInteger(sk.SecretEC)
}

func (sk *PrivateKey) UnmarshalIntegerSecret(d []byte) error {
	sk.SecretEC = sk.Curve.UnmarshalFieldInteger(d)

	if sk.SecretEC == nil {
		return goerrors.New("dilithium_ecdsa: failed to parse scalar")
	}
	return nil
}

func GenerateKey(rand io.Reader, algId uint8, c ecc.ECDSACurve, d *dilithium.Dilithium) (priv *PrivateKey, err error) {
	priv = new(PrivateKey)

	priv.PublicKey.AlgId = algId
	priv.PublicKey.Curve = c
	priv.PublicKey.Dilithium = d

	priv.PublicKey.X, priv.PublicKey.Y, priv.SecretEC, err = c.GenerateECDSA(rand)
	if err != nil {
		return nil, err
	}

	dilithiumSeed := make([]byte, dilithium.SEEDBYTES)
	_, err = rand.Read(dilithiumSeed)
	if err != nil {
		return nil, err
	}

	priv.PublicKey.PublicDilithium, priv.SecretDilithium = priv.PublicKey.Dilithium.KeyGen(dilithiumSeed)
	return
}

func Sign(rand io.Reader, priv *PrivateKey, message []byte) (dSig, ecR, ecS []byte, err error) {
	r, s, err := priv.PublicKey.Curve.Sign(rand, priv.PublicKey.X, priv.PublicKey.Y, priv.SecretEC, message)
	if err != nil {
		return nil, nil, nil, err
	}

	ecR = priv.PublicKey.Curve.MarshalFieldInteger(r)
	ecS = priv.PublicKey.Curve.MarshalFieldInteger(s)

	dSig = priv.PublicKey.Dilithium.Sign(priv.SecretDilithium, message)
	if dSig == nil {
		return nil, nil, nil, goerrors.New("dilithium_eddsa: unable to sign with dilithium")
	}

	return
}

func Verify(pub *PublicKey, message, dSig, ecR, ecS []byte) bool {
	r := pub.Curve.UnmarshalFieldInteger(ecR)
	s := pub.Curve.UnmarshalFieldInteger(ecS)

	return pub.Curve.Verify(pub.X, pub.Y, message, r, s) && pub.Dilithium.Verify(pub.PublicDilithium, message, dSig)
}

func Validate(priv *PrivateKey) (err error) {
	var tr [dilithium.SEEDBYTES]byte

	if err = priv.PublicKey.Curve.ValidateECDSA(priv.PublicKey.X, priv.PublicKey.Y, priv.SecretEC.Bytes()); err != nil {
		return err
	}

	state := sha3.NewShake256()

	state.Write(priv.PublicKey.PublicDilithium)
	state.Read(tr[:])
	kSk := priv.PublicKey.Dilithium.UnpackSK(priv.SecretDilithium)
	if subtle.ConstantTimeCompare(kSk.Tr[:], tr[:]) == 0 {
		return errors.KeyInvalidError("dilithium_eddsa: invalid public key")
	}

	return
}