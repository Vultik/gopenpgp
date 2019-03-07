package crypto

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"regexp"
	"strings"
	"time"

	"github.com/ProtonMail/go-pm-crypto/models"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	pgperrors "golang.org/x/crypto/openpgp/errors"
	"golang.org/x/crypto/openpgp/packet"

	armorUtils "github.com/ProtonMail/go-pm-crypto/armor"
)

// A keypair contains a private key and a public key.
type pmKeyObject struct {
	ID          string
	Version     int
	Flags       int
	Fingerprint string
	PublicKey   string `json:",omitempty"`
	PrivateKey  string
	//Activation string // Undocumented
	Primary int
}

// Use: only ios/android (internal)
func (ko *pmKeyObject) PrivateKeyReader() io.Reader {
	return strings.NewReader(ko.PrivateKey)
}

// Identity contains the name and the email of a key holder.
type Identity struct {
	Name  string
	Email string
}

// Signature is be used to check a signature. Because the signature is checked
// when the reader is consumed, Signature must only be used after EOF has been
// seen. A signature is only valid if s.Err() returns nil, otherwise the
// sender's identity cannot be trusted.
type Signature struct {
	md *openpgp.MessageDetails
}

type SignedString struct {
	String string
	Signed *Signature
}

var errKeyringNotUnlocked = errors.New("pmapi: cannot sign message, key ring is not unlocked")

// Use: not used by bridge
// Err returns a non-nil error if the signature is invalid.
func (s *Signature) Err() error {
	return s.md.SignatureError
}

// Use: not used by bridge
// KeyRing returns the key ring that was used to produce the signature, if
// available.
func (s *Signature) KeyRing() *KeyRing {
	if s.md.SignedBy == nil {
		return nil
	}

	return &KeyRing{
		entities: openpgp.EntityList{s.md.SignedBy.Entity},
	}
}

// Use: not used by bridge
// IsBy returns true if the signature has been created by kr's owner.
func (s *Signature) IsBy(kr *KeyRing) bool {
	// Use fingerprint if possible
	if s.md.SignedBy != nil {
		for _, e := range kr.entities {
			if e.PrimaryKey.Fingerprint == s.md.SignedBy.PublicKey.Fingerprint {
				return true
			}
		}
		return false
	}

	for _, e := range kr.entities {
		if e.PrimaryKey.KeyId == s.md.SignedByKeyId {
			return true
		}
	}
	return false
}

// A keyring contains multiple private and public keys.
type KeyRing struct {
	// PGP entities in this keyring.
	entities openpgp.EntityList
}

// Returns openpgp entities contained in this KeyRing
func (kr *KeyRing) GetEntities() openpgp.EntityList {
	return kr.entities
}

// Use: internal, but proxied to ios/android only
// Use: go-pm-crypto, message.go, sign_detached.go
func (kr *KeyRing) GetSigningEntity(passphrase string) *openpgp.Entity {

	var signEntity *openpgp.Entity

	for _, e := range kr.entities {
		// Entity.PrivateKey must be a signing key
		if e.PrivateKey != nil {
			if e.PrivateKey.Encrypted {
				e.PrivateKey.Decrypt([]byte(passphrase))
			}
			if !e.PrivateKey.Encrypted {
				signEntity = e
				break
			}
		}
	}
	return signEntity
}

// Use: go-pmapi
// Encrypt encrypts data to this keyring's owner. If sign is not nil, it also
// signs data with it. sign must be unlock to be able to sign data, if it's not
// the case an error will be returned.
func (kr *KeyRing) Encrypt(w io.Writer, sign *KeyRing, filename string, canonicalizeText bool) (io.WriteCloser, error) {
	// The API returns keys sorted by descending priority
	// Only encrypt to the first one
	var encryptEntities []*openpgp.Entity
	for _, e := range kr.entities {
		encryptEntities = append(encryptEntities, e)
		break
	}

	var signEntity *openpgp.Entity
	if sign != nil {
		// To sign a message, the private key must be decrypted
		for _, e := range sign.entities {
			// Entity.PrivateKey must be a signing key
			if e.PrivateKey != nil && !e.PrivateKey.Encrypted {
				signEntity = e
				break
			}
		}

		if signEntity == nil {
			return nil, errKeyringNotUnlocked
		}
	}

	return EncryptCore(w, encryptEntities, signEntity, filename, canonicalizeText, func() time.Time { return GetPmCrypto().GetTime() })
}

// Use: go-pm-crypto, keyring.go
// Helper common encryption method for desktop and mobile clients
func EncryptCore(w io.Writer, encryptEntities []*openpgp.Entity, signEntity *openpgp.Entity, filename string, canonicalizeText bool, timeGenerator func() time.Time) (io.WriteCloser, error) {
	config := &packet.Config{DefaultCipher: packet.CipherAES256, Time: timeGenerator}

	hints := &openpgp.FileHints{
		IsBinary: !canonicalizeText,
		FileName: filename,
	}
	if canonicalizeText {
		return openpgp.EncryptText(w, encryptEntities, signEntity, hints, config)
	} else {
		return openpgp.Encrypt(w, encryptEntities, signEntity, hints, config)
	}
}

// An io.WriteCloser that both encrypts and armors data.
type armorEncryptWriter struct {
	aw io.WriteCloser // Armored writer
	ew io.WriteCloser // Encrypted writer
}

// Encrypt data
func (w *armorEncryptWriter) Write(b []byte) (n int, err error) {
	return w.ew.Write(b)
}

// Close armor and encryption io.WriteClose
func (w *armorEncryptWriter) Close() (err error) {
	if err = w.ew.Close(); err != nil {
		return
	}
	err = w.aw.Close()
	return
}

// Use: go-pm-crypto, keyring.go
// EncryptArmored encrypts and armors data to the keyring's owner.
func (kr *KeyRing) EncryptArmored(w io.Writer, sign *KeyRing) (wc io.WriteCloser, err error) {
	aw, err := armorUtils.ArmorWithTypeBuffered(w, armorUtils.PGP_MESSAGE_HEADER)
	if err != nil {
		return
	}

	ew, err := kr.Encrypt(aw, sign, "", false)
	if err != nil {
		aw.Close()
		return
	}

	wc = &armorEncryptWriter{aw: aw, ew: ew}
	return
}

// Use go-pmapi
// EncryptString encrypts and armors a string to the keyring's owner.
func (kr *KeyRing) EncryptString(s string, sign *KeyRing) (encrypted string, err error) {
	var b bytes.Buffer
	w, err := kr.EncryptArmored(&b, sign)
	if err != nil {
		return
	}

	if _, err = w.Write([]byte(s)); err != nil {
		return
	}
	if err = w.Close(); err != nil {
		return
	}

	encrypted = b.String()
	return
}

// Use: bridge
// Encrypts data using generated symmetric key encrypted with this KeyRing
func (kr *KeyRing) EncryptSymmetric(textToEncrypt string, canonicalizeText bool) (outSplit *models.EncryptedSplit, err error) {

	var encryptedWriter io.WriteCloser
	buffer := &bytes.Buffer{}

	if encryptedWriter, err = kr.Encrypt(buffer, kr, "msg.txt", canonicalizeText); err != nil {
		return
	}

	if _, err = io.Copy(encryptedWriter, bytes.NewBufferString(textToEncrypt)); err != nil {
		return
	}
	encryptedWriter.Close()

	if outSplit, err = SeparateKeyAndData(kr, buffer, len(textToEncrypt), -1); err != nil {
		return
	}

	return
}

// Use go-pmapi
// DecryptString decrypts an armored string sent to the keypair's owner.
// If error is errors.ErrSignatureExpired (from golang.org/x/crypto/openpgp/errors),
// contents are still provided if library clients wish to process this message further
func (kr *KeyRing) DecryptString(encrypted string) (SignedString, error) {
	r, signed, err := kr.DecryptArmored(strings.NewReader(encrypted))
	if err != nil && err != pgperrors.ErrSignatureExpired {
		return SignedString{String: encrypted, Signed: nil}, err
	}

	b, err := ioutil.ReadAll(r)
	if err != nil && err != pgperrors.ErrSignatureExpired {
		return SignedString{String: encrypted, Signed: nil}, err
	}

	s := string(b)
	return SignedString{String: s, Signed: signed}, nil
}

// Use go-pmapi
// Decrypt data if has PGP MESSAGE format, if not return original data.
// If error is errors.ErrSignatureExpired (from golang.org/x/crypto/openpgp/errors),
// contents are still provided if library clients wish to process this message further
func (kr *KeyRing) DecryptStringIfNeeded(data string) (decrypted string, err error) {
	if re := regexp.MustCompile("^-----BEGIN " + armorUtils.PGP_MESSAGE_HEADER + "-----(?s:.+)-----END " + armorUtils.PGP_MESSAGE_HEADER + "-----"); re.MatchString(data) {
		var signed SignedString
		signed, err = kr.DecryptString(data)
		decrypted = signed.String
	} else {
		decrypted = data
	}
	return
}

// Use go-pmapi
// Sign a string message, using this KeyRing. canonicalizeText identifies if newlines are canonicalized
func (kr *KeyRing) SignString(message string, canonicalizeText bool) (signed string, err error) {

	var sig bytes.Buffer
	err = kr.DetachedSign(&sig, strings.NewReader(message), canonicalizeText, true)

	if err != nil {
		return "", err
	} else {
		return sig.String(), nil
	}
}

// Use: go-pmapi
// Use: go-pm-crypto, keyring.go
// Sign a separate ("detached") data from toSign, writing to w. canonicalizeText identifies if newlines are canonicalized
func (kr *KeyRing) DetachedSign(w io.Writer, toSign io.Reader, canonicalizeText bool, armored bool) (err error) {

	var signEntity *openpgp.Entity
	for _, e := range kr.entities {
		if e.PrivateKey != nil && !e.PrivateKey.Encrypted {
			signEntity = e
			break
		}
	}

	if signEntity == nil {
		return errKeyringNotUnlocked
	}

	config := &packet.Config{DefaultCipher: packet.CipherAES256,
		Time: func() time.Time {
			return GetPmCrypto().GetTime()
		},
	}

	if canonicalizeText {
		err = openpgp.ArmoredDetachSignText(w, signEntity, toSign, config)
	} else {
		if armored {
			err = openpgp.ArmoredDetachSign(w, signEntity, toSign, config)
		} else {
			err = openpgp.DetachSign(w, signEntity, toSign, config)
		}
	}
	if err != nil {
		return
	}

	return
}

// Use: go-pmapi
// May return errors.ErrSignatureExpired (defined in golang.org/x/crypto/openpgp/errors)
// In this case signature has been verified successfuly, but it is either expired or
// in the future.
func (kr *KeyRing) VerifyString(message, signature string, sign *KeyRing) (err error) {

	messageReader := strings.NewReader(message)
	signatureReader := strings.NewReader(signature)

	err = nil
	if sign != nil {
		for _, e := range sign.entities {
			if e.PrivateKey != nil && !e.PrivateKey.Encrypted {
				_, err = openpgp.CheckArmoredDetachedSignature(kr.entities, messageReader, signatureReader, nil)
				if err == nil || err == pgperrors.ErrSignatureExpired {
					return
				}
			}
		}
	}

	if err == nil {
		return errKeyringNotUnlocked
	}

	return err
}

// Use: go-pmapi
// Use: go-pm-crypto, attachment.go, message.go
// Unlock unlocks as many keys as possible with the following password. Note
// that keyrings can contain keys locked with different passwords, and thus
// err == nil does not mean that all keys have been successfully decrypted.
// If err != nil, the password is wrong for every key, and err is the last error
// encountered.
func (kr *KeyRing) Unlock(passphrase []byte) error {
	// Build a list of keys to decrypt
	var keys []*packet.PrivateKey
	for _, e := range kr.entities {
		// Entity.PrivateKey must be a signing key
		if e.PrivateKey != nil {
			keys = append(keys, e.PrivateKey)
		}

		// Entity.Subkeys can be used for encryption
		for _, subKey := range e.Subkeys {
			if subKey.PrivateKey != nil && (!subKey.Sig.FlagsValid || subKey.Sig.FlagEncryptStorage || subKey.Sig.FlagEncryptCommunications) {
				keys = append(keys, subKey.PrivateKey)
			}
		}
	}

	if len(keys) == 0 {
		return errors.New("go-pm-crypto: cannot unlock key ring, no private key available")
	}

	var err error
	var n int
	for _, key := range keys {
		if !key.Encrypted {
			continue // Key already decrypted
		}

		if err = key.Decrypt(passphrase); err == nil {
			n++
		}
	}

	if n == 0 {
		return err
	}
	return nil
}

// Use: go-pmapi
// Use: go-pm-crypto, keyring.go
// Decrypt decrypts a message sent to the keypair's owner. If the message is not
// signed, signed will be nil.
// If error is errors.ErrSignatureExpired (from golang.org/x/crypto/openpgp/errors),
// contents are still provided if library clients wish to process this message further
func (kr *KeyRing) Decrypt(r io.Reader) (decrypted io.Reader, signed *Signature, err error) {
	md, err := openpgp.ReadMessage(r, kr.entities, nil, nil)
	if err != nil && err != pgperrors.ErrSignatureExpired {
		return
	}

	decrypted = md.UnverifiedBody
	if md.IsSigned {
		signed = &Signature{md}
	}
	return
}

// Use: go-pm-crypto, keyring.go
// DecryptArmored decrypts an armored message sent to the keypair's owner.
// If error is errors.ErrSignatureExpired (from golang.org/x/crypto/openpgp/errors),
// contents are still provided if library clients wish to process this message further
func (kr *KeyRing) DecryptArmored(r io.Reader) (decrypted io.Reader, signed *Signature, err error) {
	block, err := armor.Decode(r)
	if err != nil && err != pgperrors.ErrSignatureExpired {
		return
	}

	if block.Type != armorUtils.PGP_MESSAGE_HEADER {
		err = errors.New("pmapi: not an armored PGP message")
		return
	}

	return kr.Decrypt(block.Body)
}

// Use: go-pm-crypto, keyring.go
// WriteArmoredPublicKey outputs armored public keys from the keyring to w.
func (kr *KeyRing) WriteArmoredPublicKey(w io.Writer) (err error) {
	aw, err := armor.Encode(w, openpgp.PublicKeyType, nil)
	if err != nil {
		return
	}

	for _, e := range kr.entities {
		if err = e.Serialize(aw); err != nil {
			aw.Close()
			return
		}
	}

	err = aw.Close()
	return
}

// Use: bridge
// ArmoredPublicKeyString returns the armored public keys from this keyring.
func (kr *KeyRing) ArmoredPublicKeyString() (s string, err error) {
	b := &bytes.Buffer{}
	if err = kr.WriteArmoredPublicKey(b); err != nil {
		return
	}

	s = b.String()
	return
}

// readFrom reads unarmored and armored keys from r and adds them to the keyring.
func (kr *KeyRing) readFrom(r io.Reader, armored bool) error {
	var err error
	var entities openpgp.EntityList
	if armored {
		entities, err = openpgp.ReadArmoredKeyRing(r)
	} else {
		entities, err = openpgp.ReadKeyRing(r)
	}
	for _, entity := range entities {
		if entity.PrivateKey != nil {
			switch entity.PrivateKey.PrivateKey.(type) {
			// TODO: type mismatch after crypto lib update, fix this:
			case *rsa.PrivateKey:
				//entity.PrimaryKey = packet.NewRSAPublicKey(time.Now(), entity.PrivateKey.PrivateKey.(*rsa.PrivateKey).Public().(*rsa.PublicKey))
			case *ecdsa.PrivateKey:
				entity.PrimaryKey = packet.NewECDSAPublicKey(time.Now(), entity.PrivateKey.PrivateKey.(*ecdsa.PrivateKey).Public().(*ecdsa.PublicKey))
			}
		}
		for _, subkey := range entity.Subkeys {
			if subkey.PrivateKey != nil {
				switch subkey.PrivateKey.PrivateKey.(type) {
				case *rsa.PrivateKey:
					//subkey.PublicKey = packet.NewRSAPublicKey(time.Now(), subkey.PrivateKey.PrivateKey.(*rsa.PrivateKey).Public().(*rsa.PublicKey))
				case *ecdsa.PrivateKey:
					subkey.PublicKey = packet.NewECDSAPublicKey(time.Now(), subkey.PrivateKey.PrivateKey.(*ecdsa.PrivateKey).Public().(*ecdsa.PublicKey))
				}
			}
		}
	}
	if err != nil {
		return err
	}

	if len(entities) == 0 {
		return errors.New("pmapi: key ring doesn't contain any key")
	}

	kr.entities = append(kr.entities, entities...)
	return nil
}

/*func (kr *KeyRing) AppendStringKey(key string) error {

	sr := strings.NewReader(key)
	return kr.readFrom(sr, true)
}*/

// Use: ios/android only
func (pm *PmCrypto) BuildKeyRing(binKeys []byte) (kr *KeyRing, err error) {

	kr = &KeyRing{}
	entriesReader := bytes.NewReader(binKeys)
	err = kr.readFrom(entriesReader, false)

	return
}

// Use: ios/android only
func (pm *PmCrypto) BuildKeyRingNoError(binKeys []byte) (kr *KeyRing) {

	kr = &KeyRing{}
	entriesReader := bytes.NewReader(binKeys)
	kr.readFrom(entriesReader, false)

	return
}

// Use: ios/android only
func (pm *PmCrypto) BuildKeyRingArmored(key string) (kr *KeyRing, err error) {
	keyRaw, err := armorUtils.Unarmor(key)
	keyReader := bytes.NewReader(keyRaw)
	keyEntries, err := openpgp.ReadKeyRing(keyReader)
	return &KeyRing{entities: keyEntries}, err
}

// Only ios/android
// UnmarshalJSON implements encoding/json.Unmarshaler.
func (kr *KeyRing) UnmarshalJSON(b []byte) (err error) {
	kr.entities = nil

	keyObjs := []pmKeyObject{}
	if err = json.Unmarshal(b, &keyObjs); err != nil {
		return
	}

	if len(keyObjs) == 0 {
		return
	}

	for _, ko := range keyObjs {
		kr.readFrom(ko.PrivateKeyReader(), true)
	}

	return
}

// Use: bridge
// Identities returns the list of identities associated with this key ring.
func (kr *KeyRing) Identities() []*Identity {
	var identities []*Identity
	for _, e := range kr.entities {
		for _, id := range e.Identities {
			identities = append(identities, &Identity{
				Name:  id.UserId.Name,
				Email: id.UserId.Email,
			})
		}
	}
	return identities
}

// Use: not used by bridge
// Return array of IDs of keys in this KeyRing
func (kr *KeyRing) KeyIds() []uint64 {
	var res []uint64
	for _, e := range kr.entities {
		res = append(res, e.PrimaryKey.KeyId)
	}
	return res
}

// Use: go-pmapi
// ReadArmoredKeyRing reads an armored keyring.
func ReadArmoredKeyRing(r io.Reader) (kr *KeyRing, err error) {
	kr = &KeyRing{}
	err = kr.readFrom(r, true)
	return
}

// Use: bridge
// ReadArmoredKeyRing reads an armored keyring.
func ReadKeyRing(r io.Reader) (kr *KeyRing, err error) {
	kr = &KeyRing{}
	err = kr.readFrom(r, false)
	return
}

// Use: bridge
// Take a given KeyRing list and return only those KeyRings which contain at least, one unexpired Key
// Returns only unexpired parts of these KeyRings
func FilterExpiredKeys(contactKeys []*KeyRing) (filteredKeys []*KeyRing, err error) {
	now := time.Now()
	hasExpiredEntity := false
	filteredKeys = make([]*KeyRing, 0, 0)

	for _, contactKeyRing := range contactKeys {
		keyRingHasUnexpiredEntity := false
		keyRingHasTotallyExpiredEntity := false
		for _, entity := range contactKeyRing.GetEntities() {
			hasExpired := false
			hasUnexpired := false
			for _, subkey := range entity.Subkeys {
				if subkey.Sig.KeyExpired(now) {
					hasExpired = true
				} else {
					hasUnexpired = true
				}
			}
			if hasExpired && !hasUnexpired {
				keyRingHasTotallyExpiredEntity = true
			} else if hasUnexpired {
				keyRingHasUnexpiredEntity = true
			}
		}
		if keyRingHasUnexpiredEntity {
			filteredKeys = append(filteredKeys, contactKeyRing)
		} else if keyRingHasTotallyExpiredEntity {
			hasExpiredEntity = true
		}
	}

	if len(filteredKeys) == 0 && hasExpiredEntity {
		return filteredKeys, errors.New("all contacts keys are expired")
	}

	return
}