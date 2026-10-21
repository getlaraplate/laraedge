package opcua

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/gopcua/opcua/uapolicy"

	"github.com/getlaraplate/laraedge/internal/atomicfile"
)

// The PKI of the driver lives under <data_dir>/opcua/pki/: own/ holds the
// agent's application certificate and key, trusted/ the server certificates
// the operator accepted, rejected/ the ones a connect attempt met and refused.
// The library validates no server certificate, so trust is ours (O1).
const (
	ownCertFile   = "cert.der"
	ownKeyFile    = "key.pem"
	ownRSABits    = 2048
	ownValidity   = 10 // years
	ownClockSkew  = time.Hour
	ownAppURIBase = "urn:laraplate-edge:"
	certSuffix    = ".der"
)

// Errors of the own/ directory are fixed texts: they never carry file content.
var (
	errAgentIDRequired = errors.New("opcua pki: agent id is required")
	errOwnIncomplete   = errors.New("opcua pki: own certificate directory is incomplete (certificate or key missing)")
	errOwnCertInvalid  = errors.New("opcua pki: own certificate is not a valid certificate")
	errOwnKeyInvalid   = errors.New("opcua pki: own key is not a valid PKCS#8 RSA private key")
	errOwnMismatch     = errors.New("opcua pki: own certificate and key do not match")
	errCertInvalid     = errors.New("opcua pki: not a valid certificate")
)

// thumbprintName is a stored certificate file name: 40 lowercase hex characters
// (SHA-1 of the leaf DER) and the .der suffix.
var (
	thumbprintRE     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	thumbprintFileRE = regexp.MustCompile(`^[0-9a-f]{40}\.der$`)
)

// PKIDir is the directory holding own/, trusted/ and rejected/ for dataDir.
func PKIDir(dataDir string) string {
	return filepath.Join(dataDir, "opcua", "pki")
}

// Thumbprint is the lowercase hex SHA-1 of the leaf certificate in der, the
// identity of a certificate in the trust store (40 characters).
func Thumbprint(der []byte) string {
	return hex.EncodeToString(uapolicy.Thumbprint(der))
}

// EnsureOwnCertificate returns the agent's application certificate (DER) and
// key, creating the pair under <dataDir>/opcua/pki/own/ when the directory
// does not exist (O2). The pair is written into a temporary sibling directory
// that is renamed to own/, so two processes never leave a mismatched pair; the
// loser of the rename reads the winner's pair. An own/ that exists but is
// incomplete or inconsistent is an error: it is never regenerated.
func EnsureOwnCertificate(dataDir, agentID string, now time.Time) ([]byte, *rsa.PrivateKey, error) {
	if agentID == "" {
		return nil, nil, errAgentIDRequired
	}

	pki := PKIDir(dataDir)
	own := filepath.Join(pki, "own")

	if cert, key, err := loadOwn(own); !errors.Is(err, os.ErrNotExist) {
		return cert, key, err
	}

	if err := os.MkdirAll(pki, 0o700); err != nil {
		return nil, nil, fmt.Errorf("opcua pki: create directory: %w", err)
	}

	certDER, key, keyPEM, err := generateOwn(agentID, now)
	if err != nil {
		return nil, nil, err
	}

	tmp, err := os.MkdirTemp(pki, "own-*.tmp")
	if err != nil {
		return nil, nil, fmt.Errorf("opcua pki: create temporary directory: %w", err)
	}

	renamed := false

	defer func() {
		if !renamed {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := atomicfile.Write(tmp, ownCertFile, certDER); err != nil {
		return nil, nil, err
	}

	if err := atomicfile.Write(tmp, ownKeyFile, keyPEM); err != nil {
		return nil, nil, err
	}

	if err := atomicfile.SyncDir(tmp); err != nil {
		return nil, nil, err
	}

	if err := os.Rename(tmp, own); err != nil {
		// Another process or goroutine renamed first: use its pair.
		if cert, k, loadErr := loadOwn(own); loadErr == nil {
			return cert, k, nil
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return nil, nil, loadErr
		}

		return nil, nil, fmt.Errorf("opcua pki: install own certificate: %w", err)
	}

	renamed = true

	if err := atomicfile.SyncDir(pki); err != nil {
		return nil, nil, err
	}

	return certDER, key, nil
}

// generateOwn builds a new certificate and key; the PEM is the PKCS#8 encoding
// of the key.
func generateOwn(agentID string, now time.Time) (certDER []byte, key *rsa.PrivateKey, keyPEM []byte, err error) {
	appURI, err := url.Parse(ownAppURIBase + agentID)
	if err != nil {
		return nil, nil, nil, errors.New("opcua pki: agent id cannot form an application URI")
	}

	key, err = rsa.GenerateKey(rand.Reader, ownRSABits)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opcua pki: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opcua pki: generate serial: %w", err)
	}

	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "laraplate-edge " + agentID,
			Organization: []string{"Laraplate"},
		},
		NotBefore: now.Add(-ownClockSkew),
		NotAfter:  now.AddDate(ownValidity, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment |
			x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		SignatureAlgorithm:    x509.SHA256WithRSA,
		URIs:                  []*url.URL{appURI},
	}
	if host, hostErr := os.Hostname(); hostErr == nil && host != "" {
		tpl.DNSNames = []string{host}
	}

	certDER, err = x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opcua pki: create certificate: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opcua pki: encode key: %w", err)
	}

	return certDER, key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), nil
}

// loadOwn reads the pair in own. It returns an error wrapping os.ErrNotExist
// only when the directory itself is missing; a directory without both files,
// or with files that do not parse or do not match, is a damaged pair.
func loadOwn(own string) ([]byte, *rsa.PrivateKey, error) {
	if _, err := os.Stat(own); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, os.ErrNotExist
		}

		return nil, nil, fmt.Errorf("opcua pki: inspect own certificate directory: %w", err)
	}

	certDER, certErr := os.ReadFile(filepath.Join(own, ownCertFile))
	keyPEM, keyErr := os.ReadFile(filepath.Join(own, ownKeyFile))

	if errors.Is(certErr, os.ErrNotExist) || errors.Is(keyErr, os.ErrNotExist) {
		return nil, nil, errOwnIncomplete
	}

	if certErr != nil || keyErr != nil {
		return nil, nil, errors.New("opcua pki: read own certificate files failed")
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, errOwnCertInvalid
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, nil, errOwnKeyInvalid
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, errOwnKeyInvalid
	}

	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errOwnKeyInvalid
	}

	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errOwnMismatch
	}

	return certDER, key, nil
}

// CertInfo describes a certificate of the trust store.
type CertInfo struct {
	Thumbprint string
	Subject    string
	NotAfter   time.Time
}

// TrustStore is the set of server certificates the operator accepted (O1).
// Dir is the pki directory (PKIDir): the store reads and writes its trusted/
// and rejected/ subdirectories. Expiry is reported, never enforced: the
// operator's explicit trust decides.
type TrustStore struct {
	Dir string
}

// Trusted reports whether a certificate with this thumbprint (40 lowercase
// hex characters) is in trusted/. Anything else is not trusted.
func (s TrustStore) Trusted(thumb string) bool {
	if !thumbprintRE.MatchString(thumb) {
		return false
	}

	info, err := os.Stat(filepath.Join(s.Dir, "trusted", thumb+certSuffix))

	return err == nil && info.Mode().IsRegular()
}

// Trust stores the leaf certificate of der in trusted/ and returns its
// thumbprint. Trusting a certificate twice is not an error.
func (s TrustStore) Trust(der []byte) (string, error) {
	return s.store("trusted", der)
}

// Reject stores the leaf certificate of der in rejected/, where the operator
// finds it to trust it later. It does not trust it.
func (s TrustStore) Reject(der []byte) error {
	_, err := s.store("rejected", der)

	return err
}

func (s TrustStore) store(sub string, der []byte) (string, error) {
	leaf, err := uapolicy.ParseCertificate(der)
	if err != nil {
		return "", errCertInvalid
	}

	dir := filepath.Join(s.Dir, sub)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("opcua pki: create %s directory: %w", sub, err)
	}

	thumb := Thumbprint(leaf.Raw)
	if err := atomicfile.Write(dir, thumb+certSuffix, leaf.Raw); err != nil {
		return "", err
	}

	return thumb, nil
}

// List describes the certificates in trusted/, ordered by thumbprint. Files
// whose name is not <40 hex>.der are ignored. A file that does not parse, or
// whose name is not its thumbprint, is reported in the returned error by file
// name only; the other certificates are still returned.
func (s TrustStore) List() ([]CertInfo, error) {
	dir := filepath.Join(s.Dir, "trusted")

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("opcua pki: list trusted certificates: %w", err)
	}

	var (
		infos []CertInfo
		errs  []error
	)

	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !thumbprintFileRE.MatchString(name) {
			continue
		}

		der, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			errs = append(errs, fmt.Errorf("opcua pki: trusted certificate %s cannot be read", name))

			continue
		}

		cert, parseErr := x509.ParseCertificate(der)
		if parseErr != nil {
			errs = append(errs, fmt.Errorf("opcua pki: trusted certificate %s is not a valid certificate", name))

			continue
		}

		thumb := name[:len(name)-len(certSuffix)]
		if Thumbprint(cert.Raw) != thumb {
			errs = append(errs, fmt.Errorf("opcua pki: trusted certificate %s does not match its name", name))

			continue
		}

		infos = append(infos, CertInfo{Thumbprint: thumb, Subject: cert.Subject.String(), NotAfter: cert.NotAfter})
	}

	return infos, errors.Join(errs...)
}
