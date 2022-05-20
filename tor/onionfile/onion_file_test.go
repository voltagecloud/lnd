package onionfile

import (
	"bytes"
	"io"
	"io/ioutil"
	"path/filepath"
	"testing"
)

var (
	privateKey = []byte("RSA1024 hide_me_plz")
)

type MockEncrypter struct{}

func (m MockEncrypter) EncryptPayloadToWriter(_ bytes.Buffer, _ io.Writer,
	_ []byte) error {

	return nil
}

func (m MockEncrypter) DecryptPayloadFromReader(_ io.Reader, _ []byte) ([]byte,
	error) {

	return privateKey, nil
}

// TestOnionFile tests that the File implementation of the OnionStore
// interface behaves as expected.
func TestOnionFile(t *testing.T) {
	t.Parallel()

	tempDir, err := ioutil.TempDir("", "onion_store")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}

	privateKeyPath := filepath.Join(tempDir, "secret")

	mockEncrypter := MockEncrypter{}

	// Create a new file-based onion store. A private key should not exist
	// yet.
	onionFile := NewOnionFile(
		privateKeyPath, 0600, false, mockEncrypter,
		make([]byte, 0),
	)
	if _, err := onionFile.PrivateKey(V2); err != ErrNoPrivateKey {
		t.Fatalf("expected ErrNoPrivateKey, got \"%v\"", err)
	}

	// Store the private key and ensure what's stored matches.
	if err := onionFile.StorePrivateKey(V2, privateKey); err != nil {
		t.Fatalf("unable to store private key: %v", err)
	}
	storePrivateKey, err := onionFile.PrivateKey(V2)
	if err != nil {
		t.Fatalf("unable to retrieve private key: %v", err)
	}
	if !bytes.Equal(storePrivateKey, privateKey) {
		t.Fatalf("expected private key \"%v\", got \"%v\"",
			string(privateKey), string(storePrivateKey))
	}

	// Finally, delete the private key. We should no longer be able to
	// retrieve it.
	if err := onionFile.DeletePrivateKey(V2); err != nil {
		t.Fatalf("unable to delete private key: %v", err)
	}
	if _, err := onionFile.PrivateKey(V2); err != ErrNoPrivateKey {
		t.Fatal("found deleted private key")
	}

	// Create a new file-based onion store that encrypts the key this time
	// to ensure that an encrypted key is properly handled.
	encryptedOnionFile := NewOnionFile(
		privateKeyPath, 0600, true, mockEncrypter, privateKey,
	)

	if err = encryptedOnionFile.StorePrivateKey(V2, privateKey); err != nil {
		t.Fatalf("unable to store encrypted private key: %v", err)
	}

	storedPrivateKey, err := encryptedOnionFile.PrivateKey(V2)
	if err != nil {
		t.Fatalf("unable to retrieve encrypted private key: %v", err)
	}
	if !bytes.Equal(storedPrivateKey, privateKey) {
		t.Fatalf("expected private key \"%v\", got \"%v\"",
			string(privateKey), string(storedPrivateKey))
	}

	if err := encryptedOnionFile.DeletePrivateKey(V2); err != nil {
		t.Fatalf("unable to delete private key: %v", err)
	}
}
