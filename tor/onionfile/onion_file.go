package onionfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
)

var (
	// ErrEncryptedTorPrivateKey is thrown when a tor private key is
	// encrypted, but the user requested an unencrypted key.
	ErrEncryptedTorPrivateKey = errors.New("it appears the Tor private key " +
		"is encrypted but you didn't pass the --tor.encryptkey flag. " +
		"Please restart lnd with the --tor.encryptkey flag or delete " +
		"the Tor key file for regeneration")

	// ErrNoPrivateKey is an error returned by the OnionStore.PrivateKey
	// method when a private key hasn't yet been stored.
	ErrNoPrivateKey = errors.New("private key not found")
)

// OnionType denotes the type of the onion service.
type OnionType int

const (
	// V2 denotes that the onion service is V2.
	V2 OnionType = iota

	// V3 denotes that the onion service is V3.
	V3

	// V2KeyParam is a parameter that Tor accepts for a new V2 service.
	V2KeyParam = "RSA1024"

	// V3KeyParam is a parameter that Tor accepts for a new V3 service.
	V3KeyParam = "ED25519-V3"
)

// EncrypterDecrypter is used for encrypting and decrypting the onion service
// private key.
type EncrypterDecrypter interface {
	EncryptPayloadToWriter(bytes.Buffer, io.Writer, []byte) error
	DecryptPayloadFromReader(io.Reader, []byte) ([]byte, error)
}

// File is a file-based implementation of the OnionStore interface that
// stores an onion service's private key.
type File struct {
	privateKeyPath string
	privateKeyPerm os.FileMode
	encryptKey     bool
	encrypter      EncrypterDecrypter
	keyRing        []byte
}

// NewOnionFile creates a file-based implementation of the OnionStore interface
// to store an onion service's private key.
func NewOnionFile(privateKeyPath string, privateKeyPerm os.FileMode,
	encryptKey bool, encrypter EncrypterDecrypter, keyRing []byte,
	) *File {

	return &File{
		privateKeyPath: privateKeyPath,
		privateKeyPerm: privateKeyPerm,
		encryptKey:     encryptKey,
		encrypter:      encrypter,
		keyRing:        keyRing,
	}
}

// StorePrivateKey stores the private key at its expected path. It also
// encrypts the key before storing it if requested.
func (f *File) StorePrivateKey(_ OnionType, privateKey []byte) error {
	var b bytes.Buffer
	var privateKeyContent []byte
	payload := bytes.NewBuffer(privateKey)

	if f.encryptKey {
		err := f.encrypter.EncryptPayloadToWriter(*payload, &b, f.keyRing)
		if err != nil {
			return err
		}
		privateKeyContent = b.Bytes()
	} else {
		privateKeyContent = privateKey
	}

	err := ioutil.WriteFile(
		f.privateKeyPath, privateKeyContent, f.privateKeyPerm,
	)
	if err != nil {
		return fmt.Errorf("unable to write private key "+
			"to file: %v", err)
	}
	return nil
}

// PrivateKey retrieves the private key from its expected path. If the file does
// not exist, then ErrNoPrivateKey is returned.
func (f *File) PrivateKey(_ OnionType) ([]byte, error) {
	// Try to read the Tor private key to pass into the AddOnion call
	if _, err := os.Stat(f.privateKeyPath); !errors.Is(err, os.ErrNotExist) {
		privateKeyContent, err := ioutil.ReadFile(f.privateKeyPath)
		if err != nil {
			return nil, err
		}

		// If the privateKey doesn't start with either v2 or v3 key params
		// it's likely encrypted.
		if !bytes.HasPrefix(privateKeyContent, []byte(V2KeyParam)) &&
			!bytes.HasPrefix(privateKeyContent, []byte(V3KeyParam)) {
			// If the privateKeyContent is encrypted but --tor.encryptkey
			// wasn't set we return an error
			if !f.encryptKey {
				return nil, ErrEncryptedTorPrivateKey
			}
			// Attempt to decrypt the key
			reader := bytes.NewReader(privateKeyContent)
			privateKeyContent, err = f.encrypter.DecryptPayloadFromReader(reader, f.keyRing)
			if err != nil {
				return nil, err
			}
		}
		return privateKeyContent, nil
	}
	return nil, ErrNoPrivateKey
}

// DeletePrivateKey removes the file containing the private key.
func (f *File) DeletePrivateKey(_ OnionType) error {
	return os.Remove(f.privateKeyPath)
}
