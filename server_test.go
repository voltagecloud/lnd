//go:build !rpctest
// +build !rpctest

package lnd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/stretchr/testify/require"
)

const (
	testTLSCertDuration = 42 * time.Hour
)

var (
	privateKeyPrefix = []byte("-----BEGIN EC PRIVATE KEY-----")

	privKeyBytes = channels.AlicesPrivKey

	privKey, _ = btcec.PrivKeyFromBytes(privKeyBytes)
)

// TestTLSAutoRegeneration creates an expired TLS certificate, to test that a
// new TLS certificate pair is regenerated when the old pair expires. This is
// necessary because the pair expires after a little over a year.
func TestTLSAutoRegeneration(t *testing.T) {
	// Write an expired certificate to disk.
	removeFiles, certDir, certPath, keyPath, expiredCert := writeTestCertFiles(
		t, true, false, nil,
	)
	defer func() {
		err := removeFiles(certDir)
		if err != nil {
			t.Fatalf("couldn't remove test files: %+v", err)
		}
	}()

	rpcListener := net.IPAddr{IP: net.ParseIP("127.0.0.1"), Zone: ""}
	rpcListeners := make([]net.Addr, 0)
	rpcListeners = append(rpcListeners, &rpcListener)

	// Now let's run the TLSManager's getConfig. If it works properly, it
	// should delete the cert and create a new one.
	cfg := &Config{
		TLSCertPath:     certPath,
		TLSKeyPath:      keyPath,
		TLSCertDuration: testTLSCertDuration,
		RPCListeners:    rpcListeners,
	}
	tlsManager := TLSManager{
		cfg:     cfg,
		keyRing: &mock.SecretKeyRing{},
	}
	_, _, _, cleanUp, err := tlsManager.getConfig()
	if err != nil {
		t.Fatalf("couldn't retrieve TLS config: %+v", err)
	}
	defer cleanUp()

	// Grab the certificate to test that getTLSConfig did its job correctly
	// and generated a new cert.
	newCertData, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("couldn't grab new certificate")
	}

	certDerBytes, keyBytes := genExpiredCertPair(t, tempDirPath)
	expiredCert, err := x509.ParseCertificate(certDerBytes)
	require.NoError(t, err, "failed to parse certificate")

	// Check that the expired certificate was successfully deleted and
	// replaced with a new one.
	if !newCert.NotAfter.After(expiredCert.NotAfter) {
		t.Fatalf("New certificate expiration is too old")
	}
}

// Test that the new TLS Manager loads correctly, whether the encrypted TLS key
// flag is set or not, after some refactoring.
func TestTLSManagerGenCert(t *testing.T) {
	tempDirPath, removeFiles, certPath, keyPath := newTestDirectory(t)

	cfg := &Config{
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
	}
	tlsManager := TLSManager{
		cfg: cfg,
	}

	err := tlsManager.generateOrRenewCert()
	if err != nil {
		t.Fatalf("failed to generate new certificate: %v", err)
	}

	// After this is run, a new certificate should be created and written
	// to disk. Since the TLSEncryptKey flag isn't set, we should be able
	// to read it in plaintext from disk.
	_, keyBytes, err := cert.GetCertBytesFromPath(
		cfg.TLSCertPath, cfg.TLSKeyPath,
	)
	if err != nil {
		t.Fatalf("unable to load certificate files: %v", err)
	}
	if !bytes.HasPrefix(keyBytes, privateKeyPrefix) {
		t.Fatal("key is encrypted, but shouldn't be")
	}

	err = removeFiles(tempDirPath)
	if err != nil {
		t.Fatalf("could not remove temporary files: %v", err)
	}

	// Now test that if the TLSEncryptKey flag is set, an encrypted key is
	// created and written to disk.
	tempDirPath, removeFiles, certPath, keyPath = newTestDirectory(t)
	defer func() {
		err := removeFiles(tempDirPath)
		if err != nil {
			t.Fatalf("couldn't remove test files: %+v", err)
		}
	}()

	cfg = &Config{
		TLSEncryptKey:   true,
		TLSCertPath:     certPath,
		TLSKeyPath:      keyPath,
		TLSCertDuration: testTLSCertDuration,
	}
	tlsManager = TLSManager{
		cfg: cfg,
		keyRing: &mock.SecretKeyRing{
			RootKey: privKey,
		},
	}

	err = tlsManager.generateOrRenewCert()
	if err != nil {
		t.Fatalf("failed to generate new certificate: %v", err)
	}

	_, keyBytes, err = cert.GetCertBytesFromPath(
		certPath, keyPath,
	)
	if err != nil {
		t.Fatalf("unable to load certificate files: %v", err)
	}
	if bytes.HasPrefix(keyBytes, privateKeyPrefix) {
		t.Fatal("key isn't encrypted, but should be")
	}
}

// TestTLSEncryptSetWhileKeyFileIsPlaintext tests that if we have
// cfg.TLSEncryptKey set, but the tls file saved to disk is not encrypted,
// generateOrRenewCert encrypts the file and rewrites it to disk.
func TestTLSEncryptSetWhileKeyFileIsPlaintext(t *testing.T) {
	keyRing := &mock.SecretKeyRing{
		RootKey: privKey,
	}

	// Write an unencrypted cert file to disk.
	removeFiles, certDir, certPath, keyPath, _ := writeTestCertFiles(
		t, false, false, keyRing,
	)
	defer func() {
		err := removeFiles(certDir)
		if err != nil {
			t.Fatalf("couldn't remove test files: %+v", err)
		}
	}()

	cfg := &Config{
		TLSEncryptKey: true,
		TLSCertPath:   certPath,
		TLSKeyPath:    keyPath,
	}
	tlsManager := TLSManager{
		cfg:     cfg,
		keyRing: keyRing,
	}

	// Check that the keyBytes are initially plaintext.
	_, newKeyBytes, err := cert.GetCertBytesFromPath(
		cfg.TLSCertPath, cfg.TLSKeyPath,
	)
	if err != nil {
		t.Fatalf("unable to load certificate files: %v", err)
	}
	if !bytes.HasPrefix(newKeyBytes, privateKeyPrefix) {
		t.Fatal("key doesn't have correct plaintext prefix")
	}

	// If we call the TLSManager's main function checking if a certificate
	// exists, it should detect that the TLS key is in plaintext,
	// encrypt it, and rewrite the encrypted version to disk.
	err = tlsManager.generateOrRenewCert()
	if err != nil {
		t.Fatalf("failed to generate new certificate: %v", err)
	}

	// Grab the file from disk to check that the key is no longer plaintext.
	_, newKeyBytes, err = cert.GetCertBytesFromPath(
		cfg.TLSCertPath, cfg.TLSKeyPath,
	)
	if err != nil {
		t.Fatalf("unable to load certificate files: %v", err)
	}
	if bytes.HasPrefix(newKeyBytes, privateKeyPrefix) {
		t.Fatal("key isn't encrypted, but should be")
	}
}

// TestGenerateEphemearlCert tests that an ephemeral certificate is created and
// stored to disk in a .tmp file.
func TestGenerateEphemeralCert(t *testing.T) {
	tempDirFile, removeFiles, certPath, keyPath := newTestDirectory(t)
	defer func() {
		err := removeFiles(tempDirFile)
		if err != nil {
			t.Fatalf("couldn't remove test files: %+v", err)
		}
	}()

	var emptyKeyRing keychain.KeyRing
	cfg := &Config{
		TLSCertPath:     certPath,
		TLSKeyPath:      keyPath,
		TLSEncryptKey:   true,
		TLSCertDuration: testTLSCertDuration,
	}
	tlsManager := TLSManager{
		cfg:     cfg,
		keyRing: emptyKeyRing,
	}

	err := tlsManager.generateOrRenewCert()
	if err != nil {
		t.Fatalf("failed to generate new certificate: %v", err)
	}

	// Make sure .tmp file is created at the tmp cert path.
	_, err = ioutil.ReadFile(cfg.TLSCertPath + ".tmp")
	if err != nil {
		t.Fatalf("couldn't find temp cert file: %v", err)
	}

	// But no key should be stored.
	_, err = ioutil.ReadFile(cfg.TLSKeyPath)
	if err == nil {
		t.Fatal("should have thrown an error")
	}

	// And no permanent cert file should be stored.
	_, err = ioutil.ReadFile(cfg.TLSCertPath)
	if err == nil {
		t.Fatalf("shouldn't have found a permanent cert file")
	}

	// Now test that when we reload the certificate, once we have a real
	// non-empty keyring, it generates the new certificate properly.
	keyRing := &mock.SecretKeyRing{
		RootKey: privKey,
	}
	tlsManager.keyRing = keyRing
	err = tlsManager.reloadCertificate()
	if err != nil {
		t.Fatalf("unable to reload certificate: %v", err)
	}

	// Make sure .tmp file is deleted.
	_, _, err = cert.GetCertBytesFromPath(
		cfg.TLSCertPath+".tmp", cfg.TLSKeyPath,
	)
	if err == nil {
		t.Fatal(".tmp file should have been deleted")
	}

	// Make sure a certificate now exists at the permanent cert path.
	_, _, err = cert.GetCertBytesFromPath(
		cfg.TLSCertPath, cfg.TLSKeyPath,
	)
	if err != nil {
		t.Fatalf("error loading permanent certificate: %v", err)
	}
}

// genCertPair generates a key/cert pair, with the option of generating expired
// certificates to make sure they are being regenerated correctly.
func genCertPair(t *testing.T, expired bool) ([]byte, []byte) {
	// Max serial number.
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)

	// Generate a serial number that's below the serialNumberLimit.
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	require.NoError(t, err, "failed to generate serial number")

	host := "lightning"

	// Create a simple ip address for the fake certificate.
	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

	dnsNames := []string{host, "unix", "unixpacket"}

	var notBefore, notAfter time.Time
	if expired {
		notBefore = time.Now().Add(-time.Hour * 24)
		notAfter = time.Now()
	} else {
		notBefore = time.Now()
		notAfter = time.Now().Add(time.Hour * 24)
	}

	// Construct the certificate template.
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"lnd autogenerated cert"},
			CommonName:   host,
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage: x509.KeyUsageKeyEncipherment |
			x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true, // so can sign self.
		BasicConstraintsValid: true,

		DNSNames:    dnsNames,
		IPAddresses: ipAddresses,
	}

	// Generate a private key for the certificate.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate a private key")
	}

	certDerBytes, err := x509.CreateCertificate(
		rand.Reader, &template, &template, &priv.PublicKey, priv,
	)
	require.NoError(t, err, "failed to create certificate")

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err, "unable to encode privkey")

	return certDerBytes, keyBytes
}

// writeTestCertFiles create test files and writes them to a temporary testing
// directory.
func writeTestCertFiles(t *testing.T, expiredCert, encryptTLSKey bool,
	keyRing keychain.KeyRing) (func(string) error, string, string, string,
	*x509.Certificate) {

	tempDirPath, err := ioutil.TempDir("", ".testLnd")
	if err != nil {
		t.Fatalf("couldn't create temporary cert directory")
	}
	removeFiles := os.RemoveAll

	certPath := tempDirPath + "/tls.cert"
	keyPath := tempDirPath + "/tls.key"

	var certDerBytes, keyBytes []byte
	// Either create a valid certificate or an expired certificate pair,
	// depending on the test.
	if expiredCert {
		certDerBytes, keyBytes = genCertPair(t, true)
	} else {
		certDerBytes, keyBytes = genCertPair(t, false)
	}

	parsedCert, err := x509.ParseCertificate(certDerBytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	certBuf := bytes.Buffer{}
	err = pem.Encode(
		&certBuf, &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: certDerBytes,
		},
	)
	if err != nil {
		t.Fatalf("failed to encode certificate: %v", err)
	}

	var keyBuf *bytes.Buffer
	if !encryptTLSKey {
		keyBuf = &bytes.Buffer{}
		err = pem.Encode(
			keyBuf, &pem.Block{
				Type:  "EC PRIVATE KEY",
				Bytes: keyBytes,
			},
		)
		if err != nil {
			t.Fatalf("failed to encode private key: %v", err)
		}
	} else {
		keyBuf = bytes.NewBuffer(keyBytes)
		var b bytes.Buffer
		encrypterKeyBytes, err := lnencrypt.GenEncryptionKey(keyRing)
		if err != nil {
			t.Fatalf("failed to generate encryption key: %v", err)
		}
		err = lnencrypt.Encrypter{}.EncryptPayloadToWriter(
			*keyBuf, &b, encrypterKeyBytes,
		)
		if err != nil {
			t.Fatalf("failed to encrypt private key: %v", err)
		}
	}

	// Write cert and key files.
	err = ioutil.WriteFile(tempDirPath+"/tls.cert", certBuf.Bytes(), 0644)
	if err != nil {
		t.Fatalf("failed to write cert file: %v", err)
	}
	err = ioutil.WriteFile(tempDirPath+"/tls.key", keyBuf.Bytes(), 0600)
	if err != nil {
		t.Fatalf("failed to write key file: %v", err)
	}

	return removeFiles, tempDirPath, certPath, keyPath, parsedCert
}

func newTestDirectory(t *testing.T) (string, func(string) error, string, string) {
	tempDirPath, err := ioutil.TempDir("", ".testLnd")
	if err != nil {
		t.Fatalf("couldn't create temporary cert directory")
	}
	removeFiles := os.RemoveAll

	certPath := tempDirPath + "/tls.cert"
	keyPath := tempDirPath + "/tls.key"

	return tempDirPath, removeFiles, certPath, keyPath
}

// TestShouldPeerBootstrap tests that we properly skip network bootstrap for
// the developer networks, and also if bootstrapping is explicitly disabled.
func TestShouldPeerBootstrap(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		cfg            *Config
		shouldBoostrap bool
	}{
		// Simnet active, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					SimNet: true,
				},
				Litecoin: &lncfg.Chain{},
			},
		},

		// Regtest active, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					RegTest: true,
				},
				Litecoin: &lncfg.Chain{},
			},
		},

		// Signet active, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					SigNet: true,
				},
				Litecoin: &lncfg.Chain{},
			},
		},

		// Mainnet active, but bootstrap disabled, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					MainNet: true,
				},
				Litecoin:       &lncfg.Chain{},
				NoNetBootstrap: true,
			},
		},

		// Mainnet active, should bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					MainNet: true,
				},
				Litecoin: &lncfg.Chain{},
			},
			shouldBoostrap: true,
		},

		// Testnet active, should bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					TestNet3: true,
				},
				Litecoin: &lncfg.Chain{},
			},
			shouldBoostrap: true,
		},
	}
	for i, testCase := range testCases {
		bootstrapped := shouldPeerBootstrap(testCase.cfg)
		if bootstrapped != testCase.shouldBoostrap {
			t.Fatalf("#%v: expected bootstrap=%v, got bootstrap=%v",
				i, testCase.shouldBoostrap, bootstrapped)
		}
	}
}
