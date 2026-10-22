package opcua

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var pkiNow = time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)

func ownDir(dataDir string) string { return filepath.Join(PKIDir(dataDir), "own") }

func readFile(t *testing.T, path string) []byte {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// selfSigned makes a cheap certificate (ECDSA) for the trust store tests.
func selfSigned(t *testing.T, cn string) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    pkiNow.Add(-time.Hour),
		NotAfter:     pkiNow.Add(24 * time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	return der
}

func TestThumbprint(t *testing.T) {
	der := selfSigned(t, "thumb")
	sum := sha1.Sum(der)
	want := hex.EncodeToString(sum[:])

	got := Thumbprint(der)
	if got != want {
		t.Fatalf("Thumbprint = %q, want %q", got, want)
	}

	if len(got) != 40 || got != strings.ToLower(got) {
		t.Fatalf("Thumbprint %q is not 40 lowercase hex characters", got)
	}
}

func TestEnsureOwnCertificate(t *testing.T) {
	const agentID = "agent-42"

	dataDir := t.TempDir()

	cert1, key1, err := EnsureOwnCertificate(dataDir, agentID, pkiNow)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("creates the pair with the O2 fields", func(t *testing.T) {
		cert, err := x509.ParseCertificate(cert1)
		if err != nil {
			t.Fatal(err)
		}

		if key1.N.BitLen() != 2048 {
			t.Errorf("key size = %d, want 2048", key1.N.BitLen())
		}

		if !key1.PublicKey.Equal(cert.PublicKey) {
			t.Error("the key does not match the certificate")
		}

		if len(cert.URIs) != 1 || cert.URIs[0].String() != "urn:laraplate-edge:"+agentID {
			t.Errorf("URIs = %v", cert.URIs)
		}

		if host, err := os.Hostname(); err == nil && host != "" {
			if len(cert.DNSNames) != 1 || cert.DNSNames[0] != host {
				t.Errorf("DNSNames = %v, want [%s]", cert.DNSNames, host)
			}
		}

		if cert.Subject.CommonName != "laraplate-edge "+agentID || len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "Laraplate" {
			t.Errorf("Subject = %v", cert.Subject)
		}

		wantUsage := x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment |
			x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign
		if cert.KeyUsage != wantUsage {
			t.Errorf("KeyUsage = %v, want %v", cert.KeyUsage, wantUsage)
		}

		if len(cert.ExtKeyUsage) != 2 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || cert.ExtKeyUsage[1] != x509.ExtKeyUsageClientAuth {
			t.Errorf("ExtKeyUsage = %v", cert.ExtKeyUsage)
		}

		if !cert.BasicConstraintsValid || cert.IsCA {
			t.Errorf("BasicConstraintsValid=%v IsCA=%v", cert.BasicConstraintsValid, cert.IsCA)
		}

		if cert.SignatureAlgorithm != x509.SHA256WithRSA {
			t.Errorf("SignatureAlgorithm = %v", cert.SignatureAlgorithm)
		}

		if !cert.NotBefore.Equal(pkiNow.Add(-time.Hour)) || !cert.NotAfter.Equal(pkiNow.AddDate(10, 0, 0)) {
			t.Errorf("validity = %v .. %v", cert.NotBefore, cert.NotAfter)
		}

		if cert.SerialNumber.Sign() <= 0 || cert.SerialNumber.BitLen() < 100 {
			t.Errorf("serial %v does not look like a random 128-bit number", cert.SerialNumber)
		}
	})

	t.Run("writes cert.der and a PKCS#8 key.pem", func(t *testing.T) {
		if string(readFile(t, filepath.Join(ownDir(dataDir), "cert.der"))) != string(cert1) {
			t.Error("cert.der differs from the returned certificate")
		}

		block, _ := pem.Decode(readFile(t, filepath.Join(ownDir(dataDir), "key.pem")))
		if block == nil || block.Type != "PRIVATE KEY" {
			t.Fatalf("key.pem is not a PRIVATE KEY block: %v", block)
		}

		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}

		if k, ok := parsed.(*rsa.PrivateKey); !ok || !k.Equal(key1) {
			t.Error("key.pem does not hold the returned key")
		}
	})

	t.Run("key file mode is 0600", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no POSIX file modes on Windows")
		}

		info, err := os.Stat(filepath.Join(ownDir(dataDir), "key.pem"))
		if err != nil {
			t.Fatal(err)
		}

		if info.Mode().Perm() != 0o600 {
			t.Errorf("key.pem mode = %v, want 0600", info.Mode().Perm())
		}
	})

	t.Run("a second call returns the same pair", func(t *testing.T) {
		cert2, key2, err := EnsureOwnCertificate(dataDir, agentID, pkiNow.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}

		if string(cert2) != string(cert1) || !key2.Equal(key1) {
			t.Error("the second call regenerated the pair")
		}
	})

	t.Run("damaged own directory is an error and nothing is regenerated", func(t *testing.T) {
		otherKey, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatal(err)
		}

		otherPKCS8, err := x509.MarshalPKCS8PrivateKey(otherKey)
		if err != nil {
			t.Fatal(err)
		}

		otherPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherPKCS8})
		goodKeyPEM := readFile(t, filepath.Join(ownDir(dataDir), "key.pem"))

		tests := []struct {
			name string
			cert []byte
			key  []byte
		}{
			{name: "cert without key", cert: cert1},
			{name: "key without cert", key: goodKeyPEM},
			{name: "key does not match cert", cert: cert1, key: otherPEM},
			{name: "garbage cert", cert: []byte("nope"), key: goodKeyPEM},
			{name: "garbage key", cert: cert1, key: []byte("nope")},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				if tc.cert != nil {
					writeFile(t, filepath.Join(ownDir(dir), "cert.der"), tc.cert)
				}

				if tc.key != nil {
					writeFile(t, filepath.Join(ownDir(dir), "key.pem"), tc.key)
				}

				_, _, err := EnsureOwnCertificate(dir, agentID, pkiNow)
				if err == nil {
					t.Fatal("expected an error")
				}

				if strings.Contains(err.Error(), dir) {
					t.Errorf("error leaks the path: %v", err)
				}

				entries, _ := os.ReadDir(ownDir(dir))
				for _, e := range entries {
					got := readFile(t, filepath.Join(ownDir(dir), e.Name()))
					want := map[string][]byte{"cert.der": tc.cert, "key.pem": tc.key}[e.Name()]
					if string(got) != string(want) {
						t.Errorf("%s was rewritten", e.Name())
					}
				}
			})
		}
	})

	t.Run("concurrent calls end with one matching pair", func(t *testing.T) {
		dir := t.TempDir()

		type result struct {
			cert []byte
			key  *rsa.PrivateKey
			err  error
		}

		results := make([]result, 4)

		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)

			go func() {
				defer wg.Done()

				c, k, err := EnsureOwnCertificate(dir, agentID, pkiNow)
				results[i] = result{c, k, err}
			}()
		}

		wg.Wait()

		for i, r := range results {
			if r.err != nil {
				t.Fatalf("call %d: %v", i, r.err)
			}

			if string(r.cert) != string(results[0].cert) || !r.key.Equal(results[0].key) {
				t.Errorf("call %d returned a different pair", i)
			}
		}

		cert, key, err := EnsureOwnCertificate(dir, agentID, pkiNow)
		if err != nil {
			t.Fatal(err)
		}

		if string(cert) != string(results[0].cert) || !key.Equal(results[0].key) {
			t.Error("the pair on disk is not the one the callers got")
		}

		entries, err := os.ReadDir(PKIDir(dir))
		if err != nil {
			t.Fatal(err)
		}

		if len(entries) != 1 || entries[0].Name() != "own" {
			t.Errorf("the pki directory holds leftovers: %v", entries)
		}
	})

	t.Run("an empty agent id is refused", func(t *testing.T) {
		if _, _, err := EnsureOwnCertificate(t.TempDir(), "", pkiNow); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestTrustStore(t *testing.T) {
	store := TrustStore{Dir: PKIDir(t.TempDir())}

	a := selfSigned(t, "server-a")
	b := selfSigned(t, "server-b")

	if store.Trusted(Thumbprint(a)) {
		t.Fatal("empty store trusts a certificate")
	}

	if infos, err := store.List(); err != nil || len(infos) != 0 {
		t.Fatalf("List on an empty store = %v, %v", infos, err)
	}

	thumbA, err := store.Trust(a)
	if err != nil {
		t.Fatal(err)
	}

	if thumbA != Thumbprint(a) {
		t.Errorf("Trust returned %q, want %q", thumbA, Thumbprint(a))
	}

	if !store.Trusted(thumbA) || store.Trusted(Thumbprint(b)) {
		t.Error("Trusted does not follow the trusted directory")
	}

	if string(readFile(t, filepath.Join(store.Dir, "trusted", thumbA+".der"))) != string(a) {
		t.Error("trusted file differs from the certificate")
	}

	if _, err := store.Trust(a); err != nil {
		t.Errorf("trusting twice: %v", err)
	}

	if err := store.Reject(b); err != nil {
		t.Fatal(err)
	}

	if string(readFile(t, filepath.Join(store.Dir, "rejected", Thumbprint(b)+".der"))) != string(b) {
		t.Error("rejected file differs from the certificate")
	}

	if store.Trusted(Thumbprint(b)) {
		t.Error("Reject must not trust")
	}

	thumbB, err := store.Trust(b)
	if err != nil {
		t.Fatal(err)
	}

	infos, err := store.List()
	if err != nil {
		t.Fatal(err)
	}

	if len(infos) != 2 {
		t.Fatalf("List = %v", infos)
	}

	byThumb := map[string]CertInfo{}
	for _, i := range infos {
		byThumb[i.Thumbprint] = i
	}

	if byThumb[thumbA].Subject != "CN=server-a" || byThumb[thumbB].Subject != "CN=server-b" {
		t.Errorf("subjects = %v", infos)
	}

	if !byThumb[thumbA].NotAfter.Equal(pkiNow.Add(24 * time.Hour)) {
		t.Errorf("NotAfter = %v", byThumb[thumbA].NotAfter)
	}

	t.Run("invalid input", func(t *testing.T) {
		if _, err := store.Trust([]byte("not a certificate")); err == nil {
			t.Error("Trust accepted garbage")
		}

		if err := store.Reject([]byte("not a certificate")); err == nil {
			t.Error("Reject accepted garbage")
		}

		for _, thumb := range []string{"", "../../etc/passwd", strings.ToUpper(thumbA), thumbA[:39], thumbA + "0"} {
			if store.Trusted(thumb) {
				t.Errorf("Trusted(%q) = true", thumb)
			}
		}
	})

	t.Run("list ignores foreign names and reports a damaged file by name", func(t *testing.T) {
		trusted := filepath.Join(store.Dir, "trusted")
		writeFile(t, filepath.Join(trusted, "README.txt"), []byte("hello"))
		// Not thumbA in upper case: on case-insensitive file systems (Windows) that name is the real certificate file.
		writeFile(t, filepath.Join(trusted, strings.Repeat("CD", 20)+".der"), []byte("ignored"))
		writeFile(t, filepath.Join(trusted, thumbA+".der.tmp"), []byte("ignored"))

		infos, err := store.List()
		if err != nil || len(infos) != 2 {
			t.Fatalf("List = %v, %v", infos, err)
		}

		bad := strings.Repeat("ab", 20)
		writeFile(t, filepath.Join(trusted, bad+".der"), []byte("SECRET-CONTENT"))

		infos, err = store.List()
		if err == nil {
			t.Fatal("expected an error for the damaged file")
		}

		if !strings.Contains(err.Error(), bad+".der") || strings.Contains(err.Error(), "SECRET-CONTENT") || strings.Contains(err.Error(), store.Dir) {
			t.Errorf("error = %v", err)
		}

		if len(infos) != 2 {
			t.Errorf("the good certificates must still be listed: %v", infos)
		}
	})
}
