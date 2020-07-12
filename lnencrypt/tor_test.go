package lnencrypt

import (
	"testing"

	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/stretchr/testify/require"
)

// TestTorPrivateKey tests writing and reading a private key to disk.
func TestTorPrivateKey(t *testing.T) {
	t.Parallel()

	tmpPrivateKey := "thisisasecretkey"
	tmpPrivateKeyPath := "/tmp/torprivatekey"
	keyRing := &mock.SecretKeyRing{}

	err := WriteTorPrivateKey(
		tmpPrivateKey,
		tmpPrivateKey,
		keyRing,
	)
	if err != nil {
		t.Fatalf("failed to write encrypted private key to disk")
	}

	// Should fail because the file is encrypted but requested
	// an unencrypted file.
	_, err = ReadTorPrivateKey(
		tmpPrivateKeyPath,
		false,
		keyRing,
	)
	require.NoError(
		t, err,
		"should have failed to get private key",
	)

	privateKey, err := ReadTorPrivateKey(
		tmpPrivateKeyPath,
		true,
		keyRing,
	)
	if err != nil {
		t.Fatalf("failed to read encrypted private key on disk")
	}

	if privateKey != tmpPrivateKey {
		t.Fatalf("returned private key was not what we expected")
	}
}
