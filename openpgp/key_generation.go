// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package openpgp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	goerrors "errors"
	"github.com/ProtonMail/go-crypto/openpgp/dilithium_ecdsa"
	"github.com/ProtonMail/go-crypto/openpgp/dilithium_eddsa"
	"github.com/ProtonMail/go-crypto/openpgp/sphincs_plus"
	"io"
	"math/big"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/ecdh"
	"github.com/ProtonMail/go-crypto/openpgp/ecdsa"
	"github.com/ProtonMail/go-crypto/openpgp/eddsa"
	"github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/internal/algorithm"
	"github.com/ProtonMail/go-crypto/openpgp/internal/ecc"
	"github.com/ProtonMail/go-crypto/openpgp/kyber_ecdh"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// NewEntity returns an Entity that contains a fresh RSA/RSA keypair with a
// single identity composed of the given full name, comment and email, any of
// which may be empty but must not contain any of "()<>\x00".
// If config is nil, sensible defaults will be used.
func NewEntity(name, comment, email string, config *packet.Config) (*Entity, error) {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()

	// Generate a primary signing key
	primaryPrivRaw, err := newSigner(config)
	if err != nil {
		return nil, err
	}
	primary := packet.NewSignerPrivateKey(creationTime, primaryPrivRaw)
	if config != nil && config.V5Keys {
		primary.UpgradeToV5()
	}

	e := &Entity{
		PrimaryKey: &primary.PublicKey,
		PrivateKey: primary,
		Identities: make(map[string]*Identity),
		Subkeys:    []Subkey{},
	}

	err = e.addUserId(name, comment, email, config, creationTime, keyLifetimeSecs)
	if err != nil {
		return nil, err
	}

	// NOTE: No key expiry here, but we will not return this subkey in EncryptionKey()
	// if the primary/master key has expired.
	err = e.addEncryptionSubkey(config, creationTime, 0)
	if err != nil {
		return nil, err
	}

	return e, nil
}

func (t *Entity) AddUserId(name, comment, email string, config *packet.Config) error {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()
	return t.addUserId(name, comment, email, config, creationTime, keyLifetimeSecs)
}

func (t *Entity) addUserId(name, comment, email string, config *packet.Config, creationTime time.Time, keyLifetimeSecs uint32) error {
	uid := packet.NewUserId(name, comment, email)
	if uid == nil {
		return errors.InvalidArgumentError("user id field contained invalid characters")
	}

	if _, ok := t.Identities[uid.Id]; ok {
		return errors.InvalidArgumentError("user id exist")
	}

	primary := t.PrivateKey

	isPrimaryId := len(t.Identities) == 0

	selfSignature := &packet.Signature{
		Version:           primary.PublicKey.Version,
		SigType:           packet.SigTypePositiveCert,
		PubKeyAlgo:        primary.PublicKey.PubKeyAlgo,
		Hash:              config.Hash(),
		CreationTime:      creationTime,
		KeyLifetimeSecs:   &keyLifetimeSecs,
		IssuerKeyId:       &primary.PublicKey.KeyId,
		IssuerFingerprint: primary.PublicKey.Fingerprint,
		IsPrimaryId:       &isPrimaryId,
		FlagsValid:        true,
		FlagSign:          true,
		FlagCertify:       true,
		MDC:               true, // true by default, see 5.8 vs. 5.14
		AEAD:              config.AEAD() != nil,
		V5Keys:            config != nil && config.V5Keys,
	}

	// Set the PreferredHash for the SelfSignature from the packet.Config.
	// If it is not the must-implement algorithm from rfc4880bis, append that.
	selfSignature.PreferredHash = []uint8{hashToHashId(config.Hash())}
	if config.Hash() != crypto.SHA256 {
		selfSignature.PreferredHash = append(selfSignature.PreferredHash, hashToHashId(crypto.SHA256))
	}

	// Likewise for DefaultCipher.
	selfSignature.PreferredSymmetric = []uint8{uint8(config.Cipher())}
	if config.Cipher() != packet.CipherAES128 {
		selfSignature.PreferredSymmetric = append(selfSignature.PreferredSymmetric, uint8(packet.CipherAES128))
	}

	// We set CompressionNone as the preferred compression algorithm because
	// of compression side channel attacks, then append the configured
	// DefaultCompressionAlgo if any is set (to signal support for cases
	// where the application knows that using compression is safe).
	selfSignature.PreferredCompression = []uint8{uint8(packet.CompressionNone)}
	if config.Compression() != packet.CompressionNone {
		selfSignature.PreferredCompression = append(selfSignature.PreferredCompression, uint8(config.Compression()))
	}

	// And for DefaultMode.
	selfSignature.PreferredAEAD = []uint8{uint8(config.AEAD().Mode())}
	if config.AEAD().Mode() != packet.AEADModeEAX {
		selfSignature.PreferredAEAD = append(selfSignature.PreferredAEAD, uint8(packet.AEADModeEAX))
	}

	// User ID binding signature
	err := selfSignature.SignUserId(uid.Id, &primary.PublicKey, primary, config)
	if err != nil {
		return err
	}
	t.Identities[uid.Id] = &Identity{
		Name:          uid.Id,
		UserId:        uid,
		SelfSignature: selfSignature,
		Signatures:    []*packet.Signature{selfSignature},
	}
	return nil
}

// AddSigningSubkey adds a signing keypair as a subkey to the Entity.
// If config is nil, sensible defaults will be used.
func (e *Entity) AddSigningSubkey(config *packet.Config) error {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()

	subPrivRaw, err := newSigner(config)
	if err != nil {
		return err
	}
	sub := packet.NewSignerPrivateKey(creationTime, subPrivRaw)

	subkey := Subkey{
		PublicKey:  &sub.PublicKey,
		PrivateKey: sub,
		Sig: &packet.Signature{
			Version:         e.PrimaryKey.Version,
			CreationTime:    creationTime,
			KeyLifetimeSecs: &keyLifetimeSecs,
			SigType:         packet.SigTypeSubkeyBinding,
			PubKeyAlgo:      e.PrimaryKey.PubKeyAlgo,
			Hash:            config.Hash(),
			FlagsValid:      true,
			FlagSign:        true,
			IssuerKeyId:     &e.PrimaryKey.KeyId,
			EmbeddedSignature: &packet.Signature{
				Version:      e.PrimaryKey.Version,
				CreationTime: creationTime,
				SigType:      packet.SigTypePrimaryKeyBinding,
				PubKeyAlgo:   sub.PublicKey.PubKeyAlgo,
				Hash:         config.Hash(),
				IssuerKeyId:  &e.PrimaryKey.KeyId,
			},
		},
	}
	if config != nil && config.V5Keys {
		subkey.PublicKey.UpgradeToV5()
	}

	err = subkey.Sig.EmbeddedSignature.CrossSignKey(subkey.PublicKey, e.PrimaryKey, subkey.PrivateKey, config)
	if err != nil {
		return err
	}

	subkey.PublicKey.IsSubkey = true
	subkey.PrivateKey.IsSubkey = true
	if err = subkey.Sig.SignKey(subkey.PublicKey, e.PrivateKey, config); err != nil {
		return err
	}

	e.Subkeys = append(e.Subkeys, subkey)
	return nil
}

// AddEncryptionSubkey adds an encryption keypair as a subkey to the Entity.
// If config is nil, sensible defaults will be used.
func (e *Entity) AddEncryptionSubkey(config *packet.Config) error {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()
	return e.addEncryptionSubkey(config, creationTime, keyLifetimeSecs)
}

func (e *Entity) addEncryptionSubkey(config *packet.Config, creationTime time.Time, keyLifetimeSecs uint32) error {
	subPrivRaw, err := newDecrypter(config)
	if err != nil {
		return err
	}
	sub := packet.NewDecrypterPrivateKey(creationTime, subPrivRaw)

	subkey := Subkey{
		PublicKey:  &sub.PublicKey,
		PrivateKey: sub,
		Sig: &packet.Signature{
			Version:                   e.PrimaryKey.Version,
			CreationTime:              creationTime,
			KeyLifetimeSecs:           &keyLifetimeSecs,
			SigType:                   packet.SigTypeSubkeyBinding,
			PubKeyAlgo:                e.PrimaryKey.PubKeyAlgo,
			Hash:                      config.Hash(),
			FlagsValid:                true,
			FlagEncryptStorage:        true,
			FlagEncryptCommunications: true,
			IssuerKeyId:               &e.PrimaryKey.KeyId,
		},
	}
	if config != nil && config.V5Keys {
		subkey.PublicKey.UpgradeToV5()
	}

	subkey.PublicKey.IsSubkey = true
	subkey.PrivateKey.IsSubkey = true
	if err = subkey.Sig.SignKey(subkey.PublicKey, e.PrivateKey, config); err != nil {
		return err
	}

	e.Subkeys = append(e.Subkeys, subkey)
	return nil
}

// Generates a signing key
func newSigner(config *packet.Config) (signer interface{}, err error) {
	switch config.PublicKeyAlgorithm() {
	case packet.PubKeyAlgoRSA:
		bits := config.RSAModulusBits()
		if bits < 1024 {
			return nil, errors.InvalidArgumentError("bits must be >= 1024")
		}
		if config != nil && len(config.RSAPrimes) >= 2 {
			primes := config.RSAPrimes[0:2]
			config.RSAPrimes = config.RSAPrimes[2:]
			return generateRSAKeyWithPrimes(config.Random(), 2, bits, primes)
		}
		return rsa.GenerateKey(config.Random(), bits)
	case packet.PubKeyAlgoEdDSA:
		curve := ecc.FindEdDSAByGenName(string(config.CurveName()))
		if curve == nil {
			return nil, errors.InvalidArgumentError("unsupported curve")
		}

		priv, err := eddsa.GenerateKey(config.Random(), curve)
		if err != nil {
			return nil, err
		}
		return priv, nil
	case packet.PubKeyAlgoECDSA:
		curve := ecc.FindECDSAByGenName(string(config.CurveName()))
		if curve == nil {
			return nil, errors.InvalidArgumentError("unsupported curve")
		}

		priv, err := ecdsa.GenerateKey(config.Random(), curve)
		if err != nil {
			return nil, err
		}
		return priv, nil
	case packet.PubKeyAlgoDilithium3p256, packet.PubKeyAlgoDilithium5p384, packet.PubKeyAlgoDilithium3Brainpool256,
		packet.PubKeyAlgoDilithium5Brainpool384:
		if !config.V5Keys {
			return nil, goerrors.New("openpgp: cannot create a non-v5 dilithium_ecdsa key")
		}

		c, err := packet.GetECDSACurveFromAlgID(config.PublicKeyAlgorithm())
		if err != nil {
			return nil, err
		}
		d, err := packet.GetDilithiumFromAlgID(config.PublicKeyAlgorithm())
		if err != nil {
			return nil, err
		}

		return dilithium_ecdsa.GenerateKey(config.Random(), uint8(config.PublicKeyAlgorithm()), c, d)
	case packet.PubKeyAlgoDilithium3Ed25519, packet.PubKeyAlgoDilithium5Ed448:
		if !config.V5Keys {
			return nil, goerrors.New("openpgp: cannot create a non-v5 dilithium_eddsa key")
		}

		c, err := packet.GetEdDSACurveFromAlgID(config.PublicKeyAlgorithm())
		if err != nil {
			return nil, err
		}
		d, err := packet.GetDilithiumFromAlgID(config.PublicKeyAlgorithm())
		if err != nil {
			return nil, err
		}

		return dilithium_eddsa.GenerateKey(config.Random(), uint8(config.PublicKeyAlgorithm()), c, d)
	case packet.PubKeyAlgoSphincsPlusSha2, packet.PubKeyAlgoSphincsPlusShake:
		if !config.V5Keys {
			return nil, goerrors.New("openpgp: cannot create a non-v5 sphincs+ key")
		}

		mode, err := packet.GetSphincsPlusModeFromAlgID(config.PublicKeyAlgorithm())
		if err != nil {
			return nil, err
		}
		parameter := config.SphincsPlusParam()

		return sphincs_plus.GenerateKey(config.Random(), mode, parameter)
	default:
		return nil, errors.InvalidArgumentError("unsupported public key algorithm")
	}
}

// Generates an encryption/decryption key
func newDecrypter(config *packet.Config) (decrypter interface{}, err error) {
	pubKeyAlgo := config.PublicKeyAlgorithm()
	switch pubKeyAlgo {
	case packet.PubKeyAlgoRSA:
		bits := config.RSAModulusBits()
		if bits < 1024 {
			return nil, errors.InvalidArgumentError("bits must be >= 1024")
		}
		if config != nil && len(config.RSAPrimes) >= 2 {
			primes := config.RSAPrimes[0:2]
			config.RSAPrimes = config.RSAPrimes[2:]
			return generateRSAKeyWithPrimes(config.Random(), 2, bits, primes)
		}
		return rsa.GenerateKey(config.Random(), bits)
	case packet.PubKeyAlgoEdDSA, packet.PubKeyAlgoECDSA:
		fallthrough // When passing EdDSA or ECDSA, we generate an ECDH subkey
	case packet.PubKeyAlgoECDH:
		var kdf = ecdh.KDF{
			Hash:   algorithm.SHA512,
			Cipher: algorithm.AES256,
		}
		curve := ecc.FindECDHByGenName(string(config.CurveName()))
		if curve == nil {
			return nil, errors.InvalidArgumentError("unsupported curve")
		}
		return ecdh.GenerateKey(config.Random(), curve, kdf)
	case packet.PubKeyAlgoDilithium3Ed25519, packet.PubKeyAlgoDilithium5Ed448, packet.PubKeyAlgoDilithium3p256,
		packet.PubKeyAlgoDilithium5p384, packet.PubKeyAlgoDilithium3Brainpool256,
		packet.PubKeyAlgoDilithium5Brainpool384, packet.PubKeyAlgoSphincsPlusSha2, packet.PubKeyAlgoSphincsPlusShake:
		if pubKeyAlgo, err = packet.GetMatchingKyberKem(config.PublicKeyAlgorithm()); err != nil {
			return nil, err
		}
		fallthrough // When passing Dilithium + EdDSA or ECDSA, we generate a Kyber + ECDH subkey
	case packet.PubKeyAlgoKyber768X25519, packet.PubKeyAlgoKyber1024X448, packet.PubKeyAlgoKyber768P256,
		packet.PubKeyAlgoKyber1024P384, packet.PubKeyAlgoKyber768Brainpool256, packet.PubKeyAlgoKyber1024Brainpool384:
		if !config.V5Keys {
			return nil, goerrors.New("openpgp: cannot create a non-v5 kyber_ecdh key")
		}

		c, err := packet.GetECDHCurveFromAlgID(pubKeyAlgo)
		if err != nil {
			return nil, err
		}
		k, err := packet.GetKyberFromAlgID(pubKeyAlgo)
		if err != nil {
			return nil, err
		}

		return kyber_ecdh.GenerateKey(config.Random(), uint8(pubKeyAlgo), c, k)
	default:
		return nil, errors.InvalidArgumentError("unsupported public key algorithm")
	}
}

var bigOne = big.NewInt(1)

// generateRSAKeyWithPrimes generates a multi-prime RSA keypair of the
// given bit size, using the given random source and prepopulated primes.
func generateRSAKeyWithPrimes(random io.Reader, nprimes int, bits int, prepopulatedPrimes []*big.Int) (*rsa.PrivateKey, error) {
	priv := new(rsa.PrivateKey)
	priv.E = 65537

	if nprimes < 2 {
		return nil, goerrors.New("generateRSAKeyWithPrimes: nprimes must be >= 2")
	}

	if bits < 1024 {
		return nil, goerrors.New("generateRSAKeyWithPrimes: bits must be >= 1024")
	}

	primes := make([]*big.Int, nprimes)

NextSetOfPrimes:
	for {
		todo := bits
		// crypto/rand should set the top two bits in each prime.
		// Thus each prime has the form
		//   p_i = 2^bitlen(p_i) × 0.11... (in base 2).
		// And the product is:
		//   P = 2^todo × α
		// where α is the product of nprimes numbers of the form 0.11...
		//
		// If α < 1/2 (which can happen for nprimes > 2), we need to
		// shift todo to compensate for lost bits: the mean value of 0.11...
		// is 7/8, so todo + shift - nprimes * log2(7/8) ~= bits - 1/2
		// will give good results.
		if nprimes >= 7 {
			todo += (nprimes - 2) / 5
		}
		for i := 0; i < nprimes; i++ {
			var err error
			if len(prepopulatedPrimes) == 0 {
				primes[i], err = rand.Prime(random, todo/(nprimes-i))
				if err != nil {
					return nil, err
				}
			} else {
				primes[i] = prepopulatedPrimes[0]
				prepopulatedPrimes = prepopulatedPrimes[1:]
			}

			todo -= primes[i].BitLen()
		}

		// Make sure that primes is pairwise unequal.
		for i, prime := range primes {
			for j := 0; j < i; j++ {
				if prime.Cmp(primes[j]) == 0 {
					continue NextSetOfPrimes
				}
			}
		}

		n := new(big.Int).Set(bigOne)
		totient := new(big.Int).Set(bigOne)
		pminus1 := new(big.Int)
		for _, prime := range primes {
			n.Mul(n, prime)
			pminus1.Sub(prime, bigOne)
			totient.Mul(totient, pminus1)
		}
		if n.BitLen() != bits {
			// This should never happen for nprimes == 2 because
			// crypto/rand should set the top two bits in each prime.
			// For nprimes > 2 we hope it does not happen often.
			continue NextSetOfPrimes
		}

		priv.D = new(big.Int)
		e := big.NewInt(int64(priv.E))
		ok := priv.D.ModInverse(e, totient)

		if ok != nil {
			priv.Primes = primes
			priv.N = n
			break
		}
	}

	priv.Precompute()
	return priv, nil
}
