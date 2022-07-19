// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package packet

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"encoding/binary"
	"encoding/hex"
	"github.com/ProtonMail/go-crypto/openpgp/kyber_ecdh"
	"golang.org/x/crypto/sha3"
	"io"
	"math/big"
	"strconv"

	"github.com/ProtonMail/go-crypto/openpgp/ecdh"
	"github.com/ProtonMail/go-crypto/openpgp/elgamal"
	"github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/internal/encoding"
	"github.com/ProtonMail/go-crypto/openpgp/x25519"
	"github.com/ProtonMail/go-crypto/openpgp/x448"
)

// EncryptedKey represents a public-key encrypted session key. See RFC 4880,
// section 5.1.
type EncryptedKey struct {
	Version        int
	KeyId          uint64
	KeyVersion     int    // v6
	KeyFingerprint []byte // v6
	Algo           PublicKeyAlgorithm
	CipherFunc     CipherFunction // only valid after a successful Decrypt for a v3 packet
	Key            []byte         // only valid after a successful Decrypt

	encryptedMPI1 encoding.Field // Only valid in RSA, Elgamal, ECDH, and PQC keys
	encryptedMPI2 encoding.Field // Only valid in Elgamal, ECDH and PQC keys
	encryptedMPI3 encoding.Field // Only valid in PQC keys
	ephemeralPublicX25519        *x25519.PublicKey // used for x25519
	ephemeralPublicX448          *x448.PublicKey   // used for x448
	encryptedSession             []byte            // used for x25519 and Ed448
}

func (e *EncryptedKey) parse(r io.Reader) (err error) {
	var buf [8]byte
	_, err = readFull(r, buf[:1])
	if err != nil {
		return
	}
	e.Version = int(buf[0])
	if e.Version != 3 && e.Version != 6 {
		return errors.UnsupportedError("unknown EncryptedKey version " + strconv.Itoa(int(buf[0])))
	}
	if e.Version == 6 {
		_, err = readFull(r, buf[:1])
		if err != nil {
			return
		}
		e.KeyVersion = int(buf[0])
		if e.KeyVersion != 0 && e.KeyVersion != 4 && e.KeyVersion != 6 {
			return errors.UnsupportedError("unknown public key version " + strconv.Itoa(e.KeyVersion))
		}
		var fingerprint []byte
		if e.KeyVersion == 6 {
			fingerprint = make([]byte, 32)
		} else if e.KeyVersion == 4 {
			fingerprint = make([]byte, 20)
		}
		_, err = readFull(r, fingerprint)
		if err != nil {
			return
		}
		e.KeyFingerprint = fingerprint
		if e.KeyVersion == 6 {
			e.KeyId = binary.BigEndian.Uint64(e.KeyFingerprint[:8])
		} else if e.KeyVersion == 4 {
			e.KeyId = binary.BigEndian.Uint64(e.KeyFingerprint[12:20])
		}
	} else {
		_, err = readFull(r, buf[:8])
		if err != nil {
			return
		}
		e.KeyId = binary.BigEndian.Uint64(buf[:8])
	}

	_, err = readFull(r, buf[:1])
	if err != nil {
		return
	}
	e.Algo = PublicKeyAlgorithm(buf[0])
	var cipherFunction byte
	switch e.Algo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly:
		e.encryptedMPI1 = new(encoding.MPI)
		if _, err = e.encryptedMPI1.ReadFrom(r); err != nil {
			return
		}
	case PubKeyAlgoElGamal:
		e.encryptedMPI1 = new(encoding.MPI)
		if _, err = e.encryptedMPI1.ReadFrom(r); err != nil {
			return
		}

		e.encryptedMPI2 = new(encoding.MPI)
		if _, err = e.encryptedMPI2.ReadFrom(r); err != nil {
			return
		}
	case PubKeyAlgoECDH:
		e.encryptedMPI1 = new(encoding.MPI)
		if _, err = e.encryptedMPI1.ReadFrom(r); err != nil {
			return
		}

		e.encryptedMPI2 = new(encoding.OID)
		if _, err = e.encryptedMPI2.ReadFrom(r); err != nil {
			return
		}
	case PubKeyAlgoX25519:
		e.ephemeralPublicX25519, e.encryptedSession, cipherFunction, err = x25519.DecodeFields(r, e.Version == 6)
		if err != nil {
			return
		}
	case PubKeyAlgoX448:
		e.ephemeralPublicX448, e.encryptedSession, cipherFunction, err = x448.DecodeFields(r, e.Version == 6)
		if err != nil {
			return
		}
	case PubKeyAlgoKyber768X25519:
		if err = e.readKyberECDHKey(r, 32, 1088); err != nil {
			return err
		}
	case PubKeyAlgoKyber1024X448:
		if err = e.readKyberECDHKey(r, 56, 1568); err != nil {
			return err
		}
	case PubKeyAlgoKyber768P256:
		if err = e.readKyberECDHKey(r, 65, 1088); err != nil {
			return err
		}
	case PubKeyAlgoKyber1024P384:
		if err = e.readKyberECDHKey(r, 97, 1568); err != nil {
			return err
		}
	case PubKeyAlgoKyber768Brainpool256:
		if err = e.readKyberECDHKey(r, 65, 1088); err != nil {
			return err
		}
	case PubKeyAlgoKyber1024Brainpool384:
		if err = e.readKyberECDHKey(r, 97, 1568); err != nil {
			return err
		}
	}
	if e.Version < 6 {
		switch e.Algo {
		case PubKeyAlgoX25519, PubKeyAlgoX448:
			e.CipherFunc = CipherFunction(cipherFunction)
			// Check for validiy is in the Decrypt method
		}
	}

	_, err = consumeAll(r)
	return
}

func (e *EncryptedKey) readKyberECDHKey(r io.Reader, lenEcc, lenKyber int) (err error){
	e.encryptedMPI1 = encoding.NewEmptyOctetArray(lenEcc)
	if _, err = e.encryptedMPI1.ReadFrom(r); err != nil {
		return
	}

	e.encryptedMPI2 = encoding.NewEmptyOctetArray(lenKyber)
	if _, err = e.encryptedMPI2.ReadFrom(r); err != nil {
		return
	}

	e.encryptedMPI3 = new(encoding.OID)
	if _, err = e.encryptedMPI3.ReadFrom(r); err != nil {
		return
	}

	return
}

// Decrypt decrypts an encrypted session key with the given private key. The
// private key must have been decrypted first.
// If config is nil, sensible defaults will be used.
func (e *EncryptedKey) Decrypt(priv *PrivateKey, config *Config) error {
	if e.Version < 6 && e.KeyId != 0 && e.KeyId != priv.KeyId {
		return errors.InvalidArgumentError("cannot decrypt encrypted session key for key id " + strconv.FormatUint(e.KeyId, 16) + " with private key id " + strconv.FormatUint(priv.KeyId, 16))
	}
	if e.Version == 6 && e.KeyVersion != 0 && !bytes.Equal(e.KeyFingerprint, priv.Fingerprint) {
		return errors.InvalidArgumentError("cannot decrypt encrypted session key for key fingerprint " + hex.EncodeToString(e.KeyFingerprint) + " with private key fingerprint " + hex.EncodeToString(priv.Fingerprint))
	}
	if e.Algo != priv.PubKeyAlgo {
		return errors.InvalidArgumentError("cannot decrypt encrypted session key of type " + strconv.Itoa(int(e.Algo)) + " with private key of type " + strconv.Itoa(int(priv.PubKeyAlgo)))
	}
	if priv.Dummy() {
		return errors.ErrDummyPrivateKey("dummy key found")
	}

	var err error
	var b []byte

	// TODO(agl): use session key decryption routines here to avoid
	// padding oracle attacks.
	switch priv.PubKeyAlgo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly:
		// Supports both *rsa.PrivateKey and crypto.Decrypter
		k := priv.PrivateKey.(crypto.Decrypter)
		b, err = k.Decrypt(config.Random(), padToKeySize(k.Public().(*rsa.PublicKey), e.encryptedMPI1.Bytes()), nil)
	case PubKeyAlgoElGamal:
		c1 := new(big.Int).SetBytes(e.encryptedMPI1.Bytes())
		c2 := new(big.Int).SetBytes(e.encryptedMPI2.Bytes())
		b, err = elgamal.Decrypt(priv.PrivateKey.(*elgamal.PrivateKey), c1, c2)
	case PubKeyAlgoECDH:
		vsG := e.encryptedMPI1.Bytes()
		m := e.encryptedMPI2.Bytes()
		oid := priv.PublicKey.oid.EncodedBytes()
		b, err = ecdh.Decrypt(priv.PrivateKey.(*ecdh.PrivateKey), vsG, m, oid, priv.PublicKey.Fingerprint[:])
	case PubKeyAlgoX25519:
		b, err = x25519.Decrypt(priv.PrivateKey.(*x25519.PrivateKey), e.ephemeralPublicX25519, e.encryptedSession)
	case PubKeyAlgoX448:
		b, err = x448.Decrypt(priv.PrivateKey.(*x448.PrivateKey), e.ephemeralPublicX448, e.encryptedSession)
	case PubKeyAlgoKyber768X25519, PubKeyAlgoKyber1024X448, PubKeyAlgoKyber768P256, PubKeyAlgoKyber1024P384,
		PubKeyAlgoKyber768Brainpool256, PubKeyAlgoKyber1024Brainpool384:
		ecE := e.encryptedMPI1.Bytes()
		kE := e.encryptedMPI2.Bytes()
		m := e.encryptedMPI3.Bytes()
		h := sha3.New256()
		err = priv.PublicKey.SerializeForHash(h)
		if err != nil {
			break
		}

		b, err = kyber_ecdh.Decrypt(priv.PrivateKey.(*kyber_ecdh.PrivateKey), kE, ecE, m, h.Sum(nil))
	default:
		err = errors.InvalidArgumentError("cannot decrypt encrypted session key with private key of type " + strconv.Itoa(int(priv.PubKeyAlgo)))
	}
	if err != nil {
		return err
	}

	var key []byte
	switch priv.PubKeyAlgo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly, PubKeyAlgoElGamal, PubKeyAlgoECDH:
		keyOffset := 0
		if e.Version < 6 {
			e.CipherFunc = CipherFunction(b[0])
			keyOffset = 1
			if !e.CipherFunc.IsSupported() {
				return errors.UnsupportedError("unsupported encryption function")
			}
		}
		key, err = decodeChecksumKey(b[keyOffset:])
	case PubKeyAlgoX25519, PubKeyAlgoX448:
		if e.Version < 6 {
			switch e.CipherFunc {
			case CipherAES128, CipherAES192, CipherAES256:
				break
			default:
				return errors.StructuralError("v3 PKESK mandates AES as cipher function for x25519 and x448")
			}
		}
		key = b[:]
	}
	if err != nil {
		return err
	}
	e.Key = key
	return nil
}

// Serialize writes the encrypted key packet, e, to w.
func (e *EncryptedKey) Serialize(w io.Writer) error {
	var encodedLength int
	switch e.Algo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly:
		encodedLength = int(e.encryptedMPI1.EncodedLength())
	case PubKeyAlgoElGamal:
		encodedLength = int(e.encryptedMPI1.EncodedLength()) + int(e.encryptedMPI2.EncodedLength())
	case PubKeyAlgoECDH:
		encodedLength = int(e.encryptedMPI1.EncodedLength()) + int(e.encryptedMPI2.EncodedLength())
	case PubKeyAlgoX25519:
		encodedLength = x25519.EncodedFieldsLength(e.encryptedSession, e.Version == 6)
	case PubKeyAlgoX448:
		encodedLength = x448.EncodedFieldsLength(e.encryptedSession, e.Version == 6)
	case PubKeyAlgoKyber768X25519, PubKeyAlgoKyber1024X448, PubKeyAlgoKyber768P256, PubKeyAlgoKyber1024P384,
		PubKeyAlgoKyber768Brainpool256, PubKeyAlgoKyber1024Brainpool384:
		encodedLength = int(e.encryptedMPI1.EncodedLength()) + int(e.encryptedMPI2.EncodedLength()) + int(e.encryptedMPI3.EncodedLength())
	default:
		return errors.InvalidArgumentError("don't know how to serialize encrypted key type " + strconv.Itoa(int(e.Algo)))
	}

	packetLen := 1 /* version */ + 8 /* key id */ + 1 /* algo */ + encodedLength
	if e.Version == 6 {
		packetLen = 1 /* version */ + 1 /* algo */ + encodedLength + 1 /* key version */
		if e.KeyVersion == 6 {
			packetLen += 32
		} else if e.KeyVersion == 4 {
			packetLen += 20
		}
	}

	err := serializeHeader(w, packetTypeEncryptedKey, packetLen)
	if err != nil {
		return err
	}

	_, err = w.Write([]byte{byte(e.Version)})
	if err != nil {
		return err
	}
	if e.Version == 6 {
		_, err = w.Write([]byte{byte(e.KeyVersion)})
		if err != nil {
			return err
		}
		// The key version number may also be zero,
		// and the fingerprint omitted
		if e.KeyVersion != 0 {
			_, err = w.Write(e.KeyFingerprint)
			if err != nil {
				return err
			}
		}
	} else {
		// Write KeyID
		err = binary.Write(w, binary.BigEndian, e.KeyId)
		if err != nil {
			return err
		}
	}
	_, err = w.Write([]byte{byte(e.Algo)})
	if err != nil {
		return err
	}

	switch e.Algo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly:
		_, err := w.Write(e.encryptedMPI1.EncodedBytes())
		return err
	case PubKeyAlgoElGamal:
		if _, err := w.Write(e.encryptedMPI1.EncodedBytes()); err != nil {
			return err
		}
		_, err := w.Write(e.encryptedMPI2.EncodedBytes())
		return err
	case PubKeyAlgoECDH:
		if _, err := w.Write(e.encryptedMPI1.EncodedBytes()); err != nil {
			return err
		}
		_, err := w.Write(e.encryptedMPI2.EncodedBytes())
		return err
	case PubKeyAlgoX25519:
		err := x25519.EncodeFields(w, e.ephemeralPublicX25519, e.encryptedSession, byte(e.CipherFunc), e.Version == 6)
		return err
	case PubKeyAlgoX448:
		err := x448.EncodeFields(w, e.ephemeralPublicX448, e.encryptedSession, byte(e.CipherFunc), e.Version == 6)
		return err
	case PubKeyAlgoKyber768X25519, PubKeyAlgoKyber1024X448, PubKeyAlgoKyber768P256, PubKeyAlgoKyber1024P384,
		PubKeyAlgoKyber768Brainpool256, PubKeyAlgoKyber1024Brainpool384:
		if _, err := w.Write(e.encryptedMPI1.EncodedBytes()); err != nil {
			return err
		}
		if _, err := w.Write(e.encryptedMPI2.EncodedBytes()); err != nil {
			return err
		}
		_, err := w.Write(e.encryptedMPI3.EncodedBytes())
		return err
	default:
		panic("internal error")
	}
}

// SerializeEncryptedKey serializes an encrypted key packet to w that contains
// key, encrypted to pub.
// If aeadSupported is set, PKESK v6 is used else v4.
// If config is nil, sensible defaults will be used.
func SerializeEncryptedKeyAEAD(w io.Writer, pub *PublicKey, cipherFunc CipherFunction, aeadSupported bool, key []byte, config *Config) error {
	var buf [35]byte // max possible header size is v6
	lenHeaderWritten := 1
	version := 3

	if aeadSupported {
		version = 6
	}
	// An implementation MUST NOT generate ElGamal v6 PKESKs.
	if version == 6 && pub.PubKeyAlgo == PubKeyAlgoElGamal {
		return errors.InvalidArgumentError("ElGamal v6 PKESK are not allowed")
	}
	// In v3 PKESKs, for X25519 and X448, mandate using AES
	if version == 3 && (pub.PubKeyAlgo == PubKeyAlgoX25519 || pub.PubKeyAlgo == PubKeyAlgoX448) {
		switch cipherFunc {
		case CipherAES128, CipherAES192, CipherAES256:
			break
		default:
			return errors.InvalidArgumentError("v3 PKESK mandates AES for x25519 and x448")
		}
	}

	buf[0] = byte(version)

	if version == 6 {
		if pub != nil {
			buf[1] = byte(pub.Version)
			copy(buf[2:len(pub.Fingerprint)+2], pub.Fingerprint)
			lenHeaderWritten += len(pub.Fingerprint) + 1
		} else {
			// anonymous case
			buf[1] = 0
			lenHeaderWritten += 1
		}
	} else {
		binary.BigEndian.PutUint64(buf[1:9], pub.KeyId)
		lenHeaderWritten += 8
	}
	buf[lenHeaderWritten] = byte(pub.PubKeyAlgo)
	lenHeaderWritten += 1

	var keyBlock []byte
	switch pub.PubKeyAlgo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly, PubKeyAlgoElGamal, PubKeyAlgoECDH:
		lenKeyBlock := len(key) + 2
		if version < 6 {
			lenKeyBlock += 1 // cipher type included
		}
		keyBlock = make([]byte, lenKeyBlock)
		keyOffset := 0
		if version < 6 {
			keyBlock[0] = byte(cipherFunc)
			keyOffset = 1
		}
		encodeChecksumKey(keyBlock[keyOffset:], key)
	case PubKeyAlgoX25519, PubKeyAlgoX448:
		// algorithm is added in plaintext below
		keyBlock = key
	}

	switch pub.PubKeyAlgo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly:
		return serializeEncryptedKeyRSA(w, config.Random(), buf[:lenHeaderWritten], pub.PublicKey.(*rsa.PublicKey), keyBlock)
	case PubKeyAlgoElGamal:
		return serializeEncryptedKeyElGamal(w, config.Random(), buf[:lenHeaderWritten], pub.PublicKey.(*elgamal.PublicKey), keyBlock)
	case PubKeyAlgoECDH:
		return serializeEncryptedKeyECDH(w, config.Random(), buf[:lenHeaderWritten], pub.PublicKey.(*ecdh.PublicKey), keyBlock, pub.oid, pub.Fingerprint)
	case PubKeyAlgoX25519:
		return serializeEncryptedKeyX25519(w, config.Random(), buf[:lenHeaderWritten], pub.PublicKey.(*x25519.PublicKey), keyBlock, byte(cipherFunc), version)
	case PubKeyAlgoX448:
		return serializeEncryptedKeyX448(w, config.Random(), buf[:lenHeaderWritten], pub.PublicKey.(*x448.PublicKey), keyBlock, byte(cipherFunc), version)
	case PubKeyAlgoKyber768X25519, PubKeyAlgoKyber1024X448, PubKeyAlgoKyber768P256, PubKeyAlgoKyber1024P384,
		PubKeyAlgoKyber768Brainpool256, PubKeyAlgoKyber1024Brainpool384:
		return serializeEncryptedKeyKyber(w, config.Random(), buf[:lenHeaderWritten], pub.PublicKey.(*kyber_ecdh.PublicKey), keyBlock, pub)
	case PubKeyAlgoDSA, PubKeyAlgoRSASignOnly:
		return errors.InvalidArgumentError("cannot encrypt to public key of type " + strconv.Itoa(int(pub.PubKeyAlgo)))
	}

	return errors.UnsupportedError("encrypting a key to public key of type " + strconv.Itoa(int(pub.PubKeyAlgo)))
}

// SerializeEncryptedKey serializes an encrypted key packet to w that contains
// key, encrypted to pub.
// PKESKv6 is used if config.AEAD() is not nil.
// If config is nil, sensible defaults will be used.
func SerializeEncryptedKey(w io.Writer, pub *PublicKey, cipherFunc CipherFunction, key []byte, config *Config) error {
	return SerializeEncryptedKeyAEAD(w, pub, cipherFunc, config.AEAD() != nil, key, config)
}

func serializeEncryptedKeyRSA(w io.Writer, rand io.Reader, header []byte, pub *rsa.PublicKey, keyBlock []byte) error {
	cipherText, err := rsa.EncryptPKCS1v15(rand, pub, keyBlock)
	if err != nil {
		return errors.InvalidArgumentError("RSA encryption failed: " + err.Error())
	}

	cipherMPI := encoding.NewMPI(cipherText)
	packetLen := len(header) /* header length */ + int(cipherMPI.EncodedLength())

	err = serializeHeader(w, packetTypeEncryptedKey, packetLen)
	if err != nil {
		return err
	}
	_, err = w.Write(header[:])
	if err != nil {
		return err
	}
	_, err = w.Write(cipherMPI.EncodedBytes())
	return err
}

func serializeEncryptedKeyElGamal(w io.Writer, rand io.Reader, header []byte, pub *elgamal.PublicKey, keyBlock []byte) error {
	c1, c2, err := elgamal.Encrypt(rand, pub, keyBlock)
	if err != nil {
		return errors.InvalidArgumentError("ElGamal encryption failed: " + err.Error())
	}

	packetLen := len(header) /* header length */
	packetLen += 2 /* mpi size */ + (c1.BitLen()+7)/8
	packetLen += 2 /* mpi size */ + (c2.BitLen()+7)/8

	err = serializeHeader(w, packetTypeEncryptedKey, packetLen)
	if err != nil {
		return err
	}
	_, err = w.Write(header[:])
	if err != nil {
		return err
	}
	if _, err = w.Write(new(encoding.MPI).SetBig(c1).EncodedBytes()); err != nil {
		return err
	}
	_, err = w.Write(new(encoding.MPI).SetBig(c2).EncodedBytes())
	return err
}

func serializeEncryptedKeyECDH(w io.Writer, rand io.Reader, header []byte, pub *ecdh.PublicKey, keyBlock []byte, oid encoding.Field, fingerprint []byte) error {
	vsG, c, err := ecdh.Encrypt(rand, pub, keyBlock, oid.EncodedBytes(), fingerprint)
	if err != nil {
		return errors.InvalidArgumentError("ECDH encryption failed: " + err.Error())
	}

	g := encoding.NewMPI(vsG)
	m := encoding.NewOID(c)

	packetLen := len(header) /* header length */
	packetLen += int(g.EncodedLength()) + int(m.EncodedLength())

	err = serializeHeader(w, packetTypeEncryptedKey, packetLen)
	if err != nil {
		return err
	}

	_, err = w.Write(header[:])
	if err != nil {
		return err
	}
	if _, err = w.Write(g.EncodedBytes()); err != nil {
		return err
	}
	_, err = w.Write(m.EncodedBytes())
	return err
}

func serializeEncryptedKeyX25519(w io.Writer, rand io.Reader, header []byte, pub *x25519.PublicKey, keyBlock []byte, cipherFunc byte, version int) error {
	ephemeralPublicX25519, ciphertext, err := x25519.Encrypt(rand, pub, keyBlock)
	if err != nil {
		return errors.InvalidArgumentError("X25519 encryption failed: " + err.Error())
	}

	packetLen := len(header) /* header length */
	packetLen += x25519.EncodedFieldsLength(ciphertext, version == 6)

	err = serializeHeader(w, packetTypeEncryptedKey, packetLen)
	if err != nil {
		return err
	}

	_, err = w.Write(header[:])
	if err != nil {
		return err
	}
	err = x25519.EncodeFields(w, ephemeralPublicX25519, ciphertext, cipherFunc, version == 6)
	return err
}

func serializeEncryptedKeyX448(w io.Writer, rand io.Reader, header []byte, pub *x448.PublicKey, keyBlock []byte, cipherFunc byte, version int) error {
	ephemeralPublicX448, ciphertext, err := x448.Encrypt(rand, pub, keyBlock)
	if err != nil {
		return errors.InvalidArgumentError("x448 encryption failed: " + err.Error())
	}

	packetLen := len(header) /* header length */
	packetLen += x448.EncodedFieldsLength(ciphertext, version == 6)

	err = serializeHeader(w, packetTypeEncryptedKey, packetLen)
	if err != nil {
		return err
	}

	_, err = w.Write(header[:])
	if err != nil {
		return err
	}
	err = x448.EncodeFields(w, ephemeralPublicX448, ciphertext, cipherFunc, version == 6)
	return err
}

func checksumKeyMaterial(key []byte) uint16 {
	var checksum uint16
	for _, v := range key {
		checksum += uint16(v)
	}
	return checksum
}

func decodeChecksumKey(msg []byte) (key []byte, err error) {
	key = msg[:len(msg)-2]
	expectedChecksum := uint16(msg[len(msg)-2])<<8 | uint16(msg[len(msg)-1])
	checksum := checksumKeyMaterial(key)
	if checksum != expectedChecksum {
		err = errors.StructuralError("session key checksum is incorrect")
	}
	return
}

func encodeChecksumKey(buffer []byte, key []byte) {
	copy(buffer, key)
	checksum := checksumKeyMaterial(key)
	buffer[len(key)] = byte(checksum >> 8)
	buffer[len(key)+1] = byte(checksum)
}

func serializeEncryptedKeyKyber(w io.Writer, rand io.Reader, header []byte, pub *kyber_ecdh.PublicKey, keyBlock []byte, publicKey *PublicKey) error {
	h := sha3.New256()
	publicKey.SerializeForHash(h)
	kE, ecE, c, err := kyber_ecdh.Encrypt(rand, pub, keyBlock, h.Sum(nil))
	if err != nil {
		return errors.InvalidArgumentError("kyber_ecdh encryption failed: " + err.Error())
	}

	k := encoding.NewOctetArray(kE)
	ec := encoding.NewOctetArray(ecE)
	m := encoding.NewOID(c)

	packetLen := 10 /* header length */
	packetLen += int(ec.EncodedLength()) + int(k.EncodedLength()) + int(m.EncodedLength())

	err = serializeHeader(w, packetTypeEncryptedKey, packetLen)
	if err != nil {
		return err
	}

	_, err = w.Write(header)
	if err != nil {
		return err
	}
	if _, err = w.Write(ec.EncodedBytes()); err != nil {
		return err
	}
	if _, err = w.Write(k.EncodedBytes()); err != nil {
		return err
	}
	_, err = w.Write(m.EncodedBytes())
	return err
}
