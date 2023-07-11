package client

import (
	"bufio"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha512"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/google/go-tpm/tpm2"

	attestationv1alpha1 "github.com/keylime/attestation-operator/api/attestation/v1alpha1"
	"github.com/keylime/attestation-operator/pkg/client/agent"
	"github.com/keylime/attestation-operator/pkg/client/registrar"
	"github.com/keylime/attestation-operator/pkg/client/verifier"
)

const (
	defaultTPMCertStore = "/var/lib/keylime/tpm_cert_store"
)

type Keylime interface {
	Registrar() registrar.Client
	Verifier(name string) (verifier.Client, bool)
	VerifierNames() []string
	RandomVerifier() string
	AddAgentToVerifier(ctx context.Context, agent *registrar.Agent, vc verifier.Client, payload []byte) error
	VerifyEK(ekCert *x509.Certificate) bool
}

type Client struct {
	http              *http.Client
	registrar         registrar.Client
	verifier          map[string]verifier.Client
	internalCtx       context.Context
	internalCtxCancel context.CancelFunc
	ekRootCAPool      *x509.CertPool
	acceptedHashAlgs  attestationv1alpha1.TPMHashAlgs
	acceptedEncAlgs   attestationv1alpha1.TPMEncryptionAlgs
	acceptedSignAlgs  attestationv1alpha1.TPMSigningAlgs
}

// New returns a new Keylime client which has (sort of) equivalent functionality to the Keylime tenant CLI
func New(ctx context.Context, httpClient *http.Client, registrarURL string, verifierURLs []string, tpmCertStore string) (Keylime, error) {
	internalCtx, internalCtxCancel := context.WithCancel(ctx)

	registrar, err := registrar.New(internalCtx, httpClient, registrarURL)
	if err != nil {
		internalCtxCancel()
		return nil, err
	}

	if len(verifierURLs) == 0 {
		internalCtxCancel()
		return nil, fmt.Errorf("no verifier URLs provided")
	}
	vm := make(map[string]verifier.Client, len(verifierURLs))
	for _, verifierURL := range verifierURLs {
		verifier, host, err := verifier.New(internalCtx, httpClient, verifierURL)
		if err != nil {
			internalCtxCancel()
			return nil, err
		}
		vm[host] = verifier
	}

	certStore := defaultTPMCertStore
	if tpmCertStore != "" {
		certStore = tpmCertStore
	}
	ekRootCAPool, err := readTPMCertStore(certStore)
	if err != nil {
		internalCtxCancel()
		return nil, fmt.Errorf("reading TPM cert store: %w", err)
	}

	return &Client{
		http:              httpClient,
		internalCtx:       internalCtx,
		internalCtxCancel: internalCtxCancel,
		registrar:         registrar,
		verifier:          vm,
		ekRootCAPool:      ekRootCAPool,

		// TODO: make all of these configurable
		// however, the selection here is also limited by the support in keylime itself
		acceptedHashAlgs: []attestationv1alpha1.TPMHashAlg{
			attestationv1alpha1.HashAlgSHA512,
			attestationv1alpha1.HashAlgSHA384,
			attestationv1alpha1.HashAlgSHA256,
		},
		acceptedEncAlgs: []attestationv1alpha1.TPMEncryptionAlg{
			attestationv1alpha1.EncAlgECC,
			attestationv1alpha1.EncAlgRSA,
		},
		acceptedSignAlgs: []attestationv1alpha1.TPMSigningAlg{
			attestationv1alpha1.SignAlgECSCHNORR,
			attestationv1alpha1.SignAlgRSASSA,
		},
	}, nil
}

func (c *Client) Registrar() registrar.Client {
	return c.registrar
}

func (c *Client) Verifier(name string) (verifier.Client, bool) {
	v, ok := c.verifier[name]
	return v, ok
}

func (c *Client) VerifierNames() []string {
	ret := make([]string, 0, len(c.verifier))
	for name := range c.verifier {
		ret = append(ret, name)
	}
	sort.Strings(ret)
	return ret
}

func (c *Client) RandomVerifier() string {
	names := c.VerifierNames()
	n := len(names) - 1
	if n == 0 {
		return names[0]
	}
	// if this fails, then we'll just pick always 0
	r, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if r == nil {
		r = big.NewInt(0)
	}
	return names[r.Int64()]
}

func (c *Client) AddAgentToVerifier(ctx context.Context, ragent *registrar.Agent, vc verifier.Client, payload []byte) error {
	// check regcount first: if this is > 1 we can abort right here
	if ragent.RegCount > 1 {
		return fmt.Errorf("this agent has been registered more than once! This might indicate that your system is misconfigured or a malicious host is present")
	}

	// generate K,V,U
	kvu, err := generateKVU(payload)
	if err != nil {
		return fmt.Errorf("failed to generate KVU: %w", err)
	}
	authTag, err := doHMAC(kvu.K, ragent.UUID)
	if err != nil {
		return fmt.Errorf("failed to generate auth_tag using K: %w", err)
	}

	// create agent client
	// TODO: create new HTTP client which has the right CAs etc for the agent set up
	ac, err := agent.New(ctx, c.http, fmt.Sprintf("https://%s:%d/", ragent.IP, ragent.Port))
	if err != nil {
		return fmt.Errorf("creating client for agent: %w", err)
	}

	// get agent version
	agentVersion, err := ac.GetVersion(ctx)
	if err != nil {
		return fmt.Errorf("failed to get agent version: %w", err)
	}
	if agentVersion.SupportedVersion != "2.1" {
		return fmt.Errorf("unsupported agent version %s. Supported version: 2.1", agentVersion.SupportedVersion)
	}

	// get agent quote
	// NOTE: the nonce here should be just from a select set of characters as the python implementation does it
	nonce := randomString(20)
	quote, err := ac.GetIdentityQuote(ctx, nonce)
	if err != nil {
		return fmt.Errorf("failed to get identity quote from agent: %w", err)
	}

	// check hash, encryption and signing algorithms against a list of accepted algorithms
	if !c.acceptedHashAlgs.Contains(quote.HashAlg) {
		return fmt.Errorf("unsupported hash algorithm: %s. Supported algorithms: %s", quote.HashAlg, c.acceptedHashAlgs)
	}
	if !c.acceptedEncAlgs.Contains(quote.EncryptionAlg) {
		return fmt.Errorf("unsupported encryption algorithm: %s. Supported algorithms: %s", quote.EncryptionAlg, c.acceptedEncAlgs)
	}
	if !c.acceptedSignAlgs.Contains(quote.SigningAlg) {
		return fmt.Errorf("unsupported signing algorithm: %s. Supported algorithms: %s", quote.SigningAlg, c.acceptedSignAlgs)
	}

	// verify quote
	if err := checkQuote(ragent.AIK, nonce, quote); err != nil {
		return fmt.Errorf("TPM check quote: %w", err)
	}

	// verify EK
	// TODO: this is such a random place to perform this check. This should probably just be part of the agent status itself.
	if !c.VerifyEK(ragent.EKCert) {
		return fmt.Errorf("failed to verify EK certificate")
	}

	// encrypt U with agent pubkey and post it to agent
	encryptedU, err := encryptU(kvu.U, quote.PublicKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt U using agent public key: %w", err)
	}
	if err := ac.SendUKey(ctx, &agent.SendUKeyRequest{
		AuthTag:      authTag,
		EncryptedKey: encryptedU,
		Payload:      kvu.Ciphertext,
	}); err != nil {
		return fmt.Errorf("failed to send U and payload to agent: %w", err)
	}

	// TODO: select policies
	tpmPolicy := &attestationv1alpha1.TPMPolicy{
		Mask: "0x0",
	}

	// add agent to verifier now
	if err := vc.AddAgent(ctx, ragent.UUID, &verifier.AddAgentRequest{
		V:                       kvu.V,
		CloudAgentIP:            ragent.IP,
		CloudAgentPort:          ragent.Port,
		TPMPolicy:               tpmPolicy,
		VTPMPolicy:              nil,
		RuntimePolicyName:       "",
		RuntimePolicy:           nil,
		RuntimePolicySig:        nil,
		RuntimePolicyKey:        nil,
		MBRefState:              nil,
		IMASignVerificationKeys: nil,
		MetaData:                nil,
		RevocationKey:           nil,
		AcceptTPMHashAlgs:       c.acceptedHashAlgs,
		AcceptTPMEncryptionAlgs: c.acceptedEncAlgs,
		AcceptTPMSigningAlgs:    c.acceptedSignAlgs,
		AK:                      ragent.AIK,
		MTLSCert:                ragent.MTLSCert,
		SupportedVersion:        agentVersion.SupportedVersion,
	}); err != nil {
		return fmt.Errorf("failed to add agent to verifier: %w", err)
	}

	// call agent verify
	// NOTE: the challenge here should be a "random" string and only contain the same set of characters that the python implementation uses
	challenge := randomString(20)
	expectedHMAC, err := doHMAC(kvu.K, challenge)
	if err != nil {
		return fmt.Errorf("failed to generate expected HMAC for agent challenge: %w", err)
	}
	agentHMAC, err := ac.Verify(ctx, challenge)
	if err != nil {
		return fmt.Errorf("failed to call agent verify: %w", err)
	}
	if expectedHMAC != agentHMAC {
		return fmt.Errorf("failed to verify agent: expected HMAC '%s' != agent HMAC '%s'", expectedHMAC, agentHMAC)
	}

	return nil
}

func readTPMCertStore(tpmCertStore string) (*x509.CertPool, error) {
	p := x509.NewCertPool()
	dir, err := os.Open(tpmCertStore)
	if err != nil {
		return nil, fmt.Errorf("failed to open directory %s: %w", tpmCertStore, err)
	}
	defer dir.Close()
	dirEntries, err := dir.Readdirnames(0)
	if err != nil {
		return nil, fmt.Errorf("failed to list directory entries %s: %w", tpmCertStore, err)
	}
	for _, dirEntry := range dirEntries {
		if err := func(dirEntry string) error {
			filePath := filepath.Join(tpmCertStore, dirEntry)
			f, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("failed to open file %s: %w", filePath, err)
			}
			defer f.Close()
			pemCerts, err := io.ReadAll(bufio.NewReader(f))
			if err != nil {
				return fmt.Errorf("failed to read file %s: %w", filePath, err)
			}
			p.AppendCertsFromPEM(pemCerts)
			return nil
		}(dirEntry); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (c *Client) VerifyEK(ekCert *x509.Certificate) bool {
	_, err := ekCert.Verify(x509.VerifyOptions{
		Roots: c.ekRootCAPool,
	})
	return err == nil
}

type kvu struct {
	K          []byte
	V          []byte
	U          []byte
	Ciphertext []byte
}

func generateKVU(payload []byte) (*kvu, error) {
	k := make([]byte, 32)
	v := make([]byte, 32)
	u := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("failed to generate K: %w", err)
	}
	if _, err := rand.Read(v); err != nil {
		return nil, fmt.Errorf("failed to generate V: %w", err)
	}
	for i := range k {
		u[i] = k[i] ^ v[i]
	}
	var ciphertext []byte
	if payload != nil {
		// the IV / nonce is being set to 16 bytes on the python implementation side
		// however, it's 12 bytes in Golang by default, so we need to create a new cipher with a fixed 16 byte nonce size
		iv := make([]byte, 16)
		if _, err := rand.Read(iv); err != nil {
			return nil, fmt.Errorf("failed to generate IV for K: %w", err)
		}
		kCipherBlock, err := aes.NewCipher(k)
		if err != nil {
			return nil, fmt.Errorf("failed to create AES block cipher for K: %w", err)
		}
		aesgcm, err := cipher.NewGCMWithNonceSize(kCipherBlock, 16)
		if err != nil {
			return nil, fmt.Errorf("failed to create GCM cipher for K: %w", err)
		}
		enc := aesgcm.Seal(nil, iv, payload, nil)

		// now combine the IV, ciphertext and tag as it is done in the Python implementation
		ciphertext = make([]byte, 0, len(iv)+len(enc))
		ciphertext = append(ciphertext, iv...)
		ciphertext = append(ciphertext, enc...)
		// NOTE: in Go the Seal() function returns ciphertext with the tag appended
	}
	return &kvu{
		K:          k,
		V:          v,
		U:          u,
		Ciphertext: ciphertext,
	}, nil
}

// doHMAC generates an HMAC using K and SHA-384 and returns it as a hex string like the Python implementation
func doHMAC(k []byte, agentUUID string) (string, error) {
	mac := hmac.New(sha512.New384, k)
	if _, err := mac.Write([]byte(agentUUID)); err != nil {
		return "", fmt.Errorf("failed to write agent UUID to HMAC: %w", err)
	}
	hash := mac.Sum(nil)
	return hex.EncodeToString(hash), nil
}

// encryptU is for using the agent's public key (which must be an RSA public key for the time being) to encrypt the U key.
// The python implementation is using RSA-OAEP with SHA-1 which we need to use here as well.
func encryptU(u []byte, agentPubKey crypto.PublicKey) ([]byte, error) {
	switch pubkey := agentPubKey.(type) {
	case *rsa.PublicKey:
		// the python implementation is using RSA-OAEP and SHA-1
		// NOTE: we need to use the same here
		return rsa.EncryptOAEP(sha1.New(), rand.Reader, pubkey, u, nil)
	default:
		return nil, fmt.Errorf("public key of agent is of unsupported type %T", pubkey)
	}
}

// randomString generates a random string from the character sets of a-z, A-Z and 0-9
func randomString(length int) string {
	characters := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	charactersLength := big.NewInt(int64(len(characters)))
	result := make([]byte, length)

	for i := 0; i < length; i++ {
		randomIndex, _ := rand.Int(rand.Reader, charactersLength)
		result[i] = characters[randomIndex.Int64()]
	}

	return string(result)
}

func parseAIK(b []byte) (crypto.PublicKey, error) {
	pub, err := tpm2.Unmarshal[tpm2.TPM2BPublic](b)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal TPM2B_PUBLIC struct from bytes: %w", err)
	}
	c, err := pub.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to get contents of TPM2B_PUBLIC from unmarshaled contents: %w", err)
	}
	switch c.Type {
	case tpm2.TPMAlgRSA:
		rsaDetail, err := c.Parameters.RSADetail()
		if err != nil {
			return nil, fmt.Errorf("failed to get RSA parameters of TPM2B_PUBLIC: %w", err)
		}
		rsaPubKey, err := c.Unique.RSA()
		if err != nil {
			return nil, fmt.Errorf("failed to get RSA public key of TPM2B_PUBLIC: %w", err)
		}
		pub, err := tpm2.RSAPub(rsaDetail, rsaPubKey)
		if err != nil {
			return nil, fmt.Errorf("failed to create RSA public key from TPM2B_PUBLIC parameters and key: %w", err)
		}
		return pub, nil
	case tpm2.TPMAlgECC:
		eccDetail, err := c.Parameters.ECCDetail()
		if err != nil {
			return nil, fmt.Errorf("failed to get ECC parameters of TPM2B_PUBLIC: %w", err)
		}
		eccPoint, err := c.Unique.ECC()
		if err != nil {
			return nil, fmt.Errorf("failed to get ECC public key of TPM2B_PUBLIC: %w", err)
		}
		pub, err := tpm2.ECCPub(eccDetail, eccPoint)
		if err != nil {
			return nil, fmt.Errorf("failed to create ECC public key from TPM2B_PUBLIC parameters and key: %w", err)
		}
		return pub, nil
	default:
		return nil, fmt.Errorf("unsupported TPM2B_PUBLIC type 0x%x", c.Type)
	}
}

func checkQuote(aikFromRegistrar []byte, nonce string, quote *agent.IdentityQuote) error {
	aik, err := parseAIK(aikFromRegistrar)
	if err != nil {
		return err
	}

	// check signature
	sigAlg, hashAlg, signature, err := parseSigblob(quote.Quote.TPMSig)
	if err != nil {
		return fmt.Errorf("failed to parse TPM signature blog: %w", err)
	}
	hash, err := hashAlg.Hash()
	if err != nil {
		return fmt.Errorf("TPM signature is using unsupported hash algorithm: %w", err)
	}
	digest := hash.New()
	digest.Write(quote.Quote.TPMQuote)
	quote_digest := digest.Sum(nil)

	switch aikPub := aik.(type) {
	case *rsa.PublicKey:
		if sigAlg != tpm2.TPMAlgRSASSA {
			return fmt.Errorf("unsupported TPM signature algorithm for RSA keys: 0x%x", sigAlg)
		}
		if err := rsa.VerifyPKCS1v15(aikPub, hash, quote_digest, signature); err != nil {
			return fmt.Errorf("signature verification failed: %w", err)
		}
	default:
		return fmt.Errorf("unsupport AIK public key type: %T", aikPub)
	}

	// check nonce in quote
	tpmsAttest, err := parseQuoteblob(quote.Quote.TPMQuote)
	if err != nil {
		return err
	}
	if nonce != string(tpmsAttest.ExtraData.Buffer) {
		return fmt.Errorf("nonce mismatch: nonce %s != TPMS_ATTEST.ExtraData(%s)", nonce, string(tpmsAttest.ExtraData.Buffer))
	}

	// check PCR digest in quote against PCR blob
	attestedQuoteInfo, err := tpmsAttest.Attested.Quote()
	if err != nil {
		return fmt.Errorf("failed to get attested quote from TPMS_ATTEST: %w", err)
	}
	pcrSelectCount, tpmlPCRSelection, pcrValuesList, err := parsePCRsBlob(quote.Quote.TPMPCRs)
	if err != nil {
		return fmt.Errorf("parsing PCRs blob: %w", err)
	}
	pcrQuoteDigest, pcrValues, err := hashPCRBanks(hashAlg, pcrSelectCount, tpmlPCRSelection, pcrValuesList)
	if err != nil {
		return fmt.Errorf("hashing PCR banks: %w", err)
	}
	if !reflect.DeepEqual(attestedQuoteInfo.PCRDigest.Buffer, pcrQuoteDigest) {
		return fmt.Errorf("the digest used for quoting is different than the one that was calculated")
	}
	if len(pcrValues) == 0 {
		return fmt.Errorf("quote does not contain any PCRs. Ensure the TPM supports %s PCR banks", hash.String())
	}

	// check that correct quote_digest was used which is equivalent to hash(quoteblob)
	// TODO: implement.... also confusing and looks like a bug in the python code?

	// check TPM clock info
	if !tpmsAttest.ClockInfo.Safe {
		return fmt.Errorf("TPM clock safe flag is disabled")
	}
	// NOTE: I don't think it makes sense here to try to check all the things that the python implementation is checking
	// as we are only getting a quote once

	// check PCRs - that's essentially a policy check against the PCRs
	// TODO: implement this once we're adding policies

	// all done! - the quote is gooooood
	return nil
}

func parseSigblob(sigblob []byte) (tpm2.TPMAlgID, tpm2.TPMIAlgHash, []byte, error) {
	b := sigblob
	if len(sigblob) < 7 {
		return 0, 0, nil, fmt.Errorf("length of sigblob < 7")
	}
	sigAlg := binary.BigEndian.Uint16(b[0:2])
	hashAlg := binary.BigEndian.Uint16(b[2:4])
	sigSize := binary.BigEndian.Uint16(b[4:6])
	bytesRemaining := len(b[6:])
	if bytesRemaining < int(sigSize) {
		return 0, 0, nil, fmt.Errorf("bytesRemaining %d < sigSize %d", bytesRemaining, sigSize)
	}
	return tpm2.TPMAlgID(sigAlg), tpm2.TPMAlgID(hashAlg), b[6:(sigSize + 6)], nil
}

func parseQuoteblob(quoteblob []byte) (*tpm2.TPMSAttest, error) {
	tpmsAttest, err := tpm2.Unmarshal[tpm2.TPMSAttest](quoteblob)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal TPM quote as TPMS_ATTEST: %w", err)
	}
	return tpmsAttest, nil
}

var errNotLargeEnough = errors.New("PCRs blob is not large enough")

func parsePCRsBlob(pcrsblob []byte) (uint32, map[tpm2.TPMIAlgHash]uint32, [][]byte, error) {
	tpmlPCRSelection := make(map[tpm2.TPMIAlgHash]uint32)
	b := pcrsblob
	if len(b) < 4 {
		return 0, nil, nil, errNotLargeEnough
	}
	// the first uint32 is the PCR Select count
	// NOTE: this is an tpm2-tools specific format and is LittleEndian in this case
	// TODO: figure out if this could be architecture dependent
	pcrSelectCount := binary.LittleEndian.Uint32(b[0:4])
	b = b[4:]
	for i := 0; i < 16; i++ {
		if len(b) < 2 {
			return 0, nil, nil, errNotLargeEnough
		}
		hashAlg := binary.LittleEndian.Uint16(b[0:2])
		b = b[2:]
		if len(b) < 1 {
			return 0, nil, nil, errNotLargeEnough
		}
		sizeOfSelect := uint8(b[0])
		b = b[1:]
		if sizeOfSelect != 0 && sizeOfSelect != 3 {
			return 0, nil, nil, fmt.Errorf("sizeOfSelect must be either 0 or 3, bit it is %d", sizeOfSelect)
		}

		var pcrSelect uint32
		// we always need to advance by sizeOfSelect == 3 and 2 bytes alignment
		if len(b) < 5 {
			return 0, nil, nil, errNotLargeEnough
		}
		if sizeOfSelect == 3 {
			pcrSelect = uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		} else {
			pcrSelect = 0
		}
		tpmlPCRSelection[tpm2.TPMIAlgHash(hashAlg)] = pcrSelect
		b = b[5:]
	}

	// number of subsequent TPML_DIGESTs
	if len(b) < 4 {
		return 0, nil, nil, errNotLargeEnough
	}
	pcrsCount := binary.LittleEndian.Uint32(b[0:4])
	b = b[4:]

	var pcrValues [][]byte
	for i := 0; i < int(pcrsCount); i++ {
		// TPML_DIGEST::count
		if len(b) < 4 {
			return 0, nil, nil, errNotLargeEnough
		}
		// not used?
		_ = binary.LittleEndian.Uint32(b[0:4])
		b = b[4:]

		// TPML_DIGEST::TPM2B_DIGEST[8]
		for j := 0; j < 8; j++ {
			if len(b) < 2 {
				return 0, nil, nil, errNotLargeEnough
			}
			sz := binary.LittleEndian.Uint16(b[0:2])
			b = b[2:]

			// we'll always advance by the size of TPMU_HA (= size of SHA512) below
			if len(b) < 64 {
				return 0, nil, nil, errNotLargeEnough
			}
			if sz > 0 {
				pcrValue := make([]byte, sz)
				copy(pcrValue, b[:sz])
				pcrValues = append(pcrValues, pcrValue)
			}
			b = b[64:]
		}
	}

	if len(b) != 0 {
		return 0, nil, nil, fmt.Errorf("failed to parse entire PCRs blob: %d bytes left", len(b))
	}

	return pcrSelectCount, tpmlPCRSelection, pcrValues, nil
}

func hashPCRBanks(hashAlg tpm2.TPMIAlgHash, pcrSelectCount uint32, tpmlPCRSelection map[tpm2.TPMIAlgHash]uint32, pcrValues [][]byte) ([]byte, map[uint8]string, error) {
	hash, err := hashAlg.Hash()
	if err != nil {
		return nil, nil, err
	}
	digest := hash.New()
	m := make(map[uint8]string)

	idx := 0
	for i := 0; i < int(pcrSelectCount); i++ {
		for pcrID := 0; pcrID < 24; pcrID++ {
			if tpmlPCRSelection[hashAlg]&(1<<pcrID) == 0 {
				continue
			}
			if idx >= len(pcrValues) {
				return nil, nil, fmt.Errorf("PCR values list is too short to get item at index %d of %d items", idx, len(pcrValues))
			}
			if _, err := digest.Write(pcrValues[idx]); err != nil {
				return nil, nil, fmt.Errorf("failed to update hash: %w", err)
			}
			m[uint8(pcrID)] = hex.EncodeToString(pcrValues[idx])
			idx += 1
		}
	}

	if idx != len(pcrValues) {
		return nil, nil, fmt.Errorf("did not consume all entries in the PCR values list")
	}

	// finish the hash and return
	quote_digest := digest.Sum(nil)
	return quote_digest, m, nil
}

/*
def __hash_pcr_banks(
    hash_alg: int, pcr_select_count: int, tpml_pcr_selection: Dict[int, int], pcr_values: List[bytes]
) -> Tuple[bytes, Dict[int, str]]:
    """From the tpml_pcr_selection determine which PCRs were quoted and hash these PCRs to get
    the hash that was used for the quote. Build a dict that contains the PCR values."""
    hashfunc = tpm2_objects.HASH_FUNCS.get(hash_alg)
    if not hashfunc:
        raise ValueError(f"Unsupported hash with id {hash_alg:#x} in signature blob")

    digest = hashes.Hash(hashfunc, backend=backends.default_backend())

    idx = 0
    pcrs_dict: Dict[int, str] = {}

    for _ in range(0, pcr_select_count):
        for pcr_id in range(0, 24):
            if tpml_pcr_selection[hash_alg] & (1 << pcr_id) == 0:
                continue
            if idx >= len(pcr_values):
                raise ValueError(f"pcr_values list is too short to get item {idx}")
            digest.update(pcr_values[idx])
            pcrs_dict[pcr_id] = pcr_values[idx].hex()
            idx = idx + 1

    if idx != len(pcr_values):
        raise ValueError("Did not consume all entries in pcr_values list")

    quote_digest = digest.finalize()

    return quote_digest, pcrs_dict
*/