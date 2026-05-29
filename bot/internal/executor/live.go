package executor

// LiveExecutor — places real orders on the Polymarket CLOB.
//
// Order flow:
//  1. Build order parameters (salt, amounts in micro-units)
//  2. Sign with EIP-712 TypedDataSign (ERC-7739) using the EOA private key
//  3. POST signed order to clob.polymarket.com/order with L2 auth headers
//  4. Parse fill response and log the result
//
// EIP-712 domain: "Polymarket CTF Exchange" v2, Polygon (chainId=137)
// Exchange V2:    CTF Exchange — 0xE111180000d2663C0091e4f400237545B87B996B
//
// V2 order format (new as of May 2026):
//   - Protocol version "2"
//   - Deposit wallet (proxy wallet = UUPS deposit wallet) is both maker and signer
//   - signatureType = 3 (POLY_1271 — ERC-7739 TypedDataSign wrapping)
//   - Order struct: salt, maker, signer, tokenId, makerAmount, takerAmount,
//     side, signatureType, timestamp, metadata, builder
//   - Signature is ERC-7739 wrapped: 65-byte ECDSA + appDomainSep + orderHash + typeStr + typeLen

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/w0kki/polymarket-bot/internal/db"
	"github.com/w0kki/polymarket-bot/internal/kelly"
	"github.com/w0kki/polymarket-bot/internal/market"
)

const (
	clobBaseURL = "https://clob.polymarket.com"

	// V2 exchange contract addresses (no 0x prefix).
	ctfExchangeAddr        = "E111180000d2663C0091e4f400237545B87B996B" // standard exchange
	ctfNegRiskExchangeAddr = "e2222d279d744050d28e00520010520000310F59" // neg-risk exchange

	polygonChainID = int64(137)

	// EIP-712 type strings — V2 format.
	domainTypeSig = "EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"
	orderTypeSig  = "Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 metadata,bytes32 builder)"

	// TypedDataSign type string (ERC-7739). Includes Order as a dependent type.
	typedDataSignTypeSig = "TypedDataSign(Order contents,string name,string version,uint256 chainId,address verifyingContract,bytes32 salt)" +
		"Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 metadata,bytes32 builder)"

	protocolVersion = "2"
)

// bytes32Zero is 32 zero bytes — used for metadata and builder fields.
var bytes32Zero = make([]byte, 32)

// LiveExecutor places real orders on the Polymarket CLOB via EIP-712 signed
// limit orders submitted as Fill-or-Kill (FOK) — they fill at the observed
// price or are cancelled immediately, preventing partial fills.
//
// V2 architecture: the proxy wallet (= UUPS deposit wallet) is both maker and
// signer. Orders are signed using ERC-7739 TypedDataSign wrapping (signatureType 3).
type LiveExecutor struct {
	privateKey       []byte // 32-byte secp256k1 private key
	address          string // checksummed EOA address derived from private key
	proxyWallet      string // deposit wallet address (= old proxy wallet, now UUPS deposit wallet)
	apiKey           string
	apiSecret        []byte // base64-decoded HMAC signing key
	apiPassphrase    string
	client           *http.Client
	domainSep        []byte // pre-computed EIP-712 domain separator — standard exchange
	domainSepNegRisk []byte // pre-computed EIP-712 domain separator — neg-risk exchange
	db               *db.DB // shared SQLite DB — write trade record immediately after fill
}

// NewLive constructs a LiveExecutor from credentials and the shared database.
func NewLive(privateKeyHex, apiKey, apiSecret, passphrase, proxyWallet string, database *db.DB) (*LiveExecutor, error) {
	if privateKeyHex == "" || apiKey == "" || apiSecret == "" || passphrase == "" {
		return nil, fmt.Errorf("live executor: POLY_PRIVATE_KEY, POLY_API_KEY, POLY_API_SECRET and POLY_API_PASSPHRASE must all be set")
	}
	if proxyWallet == "" {
		return nil, fmt.Errorf("live executor: POLY_PROXY_WALLET must be set (your Polymarket deposit/proxy wallet address)")
	}

	keyHex := strings.TrimPrefix(privateKeyHex, "0x")
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil || len(keyBytes) != 32 {
		return nil, fmt.Errorf("live executor: invalid POLY_PRIVATE_KEY (must be 32-byte hex): %w", err)
	}

	privKey, err := gethcrypto.ToECDSA(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("live executor: cannot parse private key: %w", err)
	}
	address := gethcrypto.PubkeyToAddress(privKey.PublicKey).Hex()

	// Decode the API secret — Polymarket emits base64url (with - and _).
	secretBytes, err := base64.URLEncoding.DecodeString(apiSecret)
	if err != nil {
		secretBytes, err = base64.StdEncoding.DecodeString(apiSecret)
		if err != nil {
			return nil, fmt.Errorf("live executor: cannot decode POLY_API_SECRET (expected base64): %w", err)
		}
	}

	log.Printf("[live] executor ready — EOA (signer): %s", address)
	log.Printf("[live] executor ready — deposit wallet (maker+signer): %s", proxyWallet)

	exe := &LiveExecutor{
		privateKey:    keyBytes,
		address:       address,
		proxyWallet:   proxyWallet,
		apiKey:        apiKey,
		apiSecret:     secretBytes,
		apiPassphrase: passphrase,
		client:        &http.Client{Timeout: 15 * time.Second},
		db:            database,
	}
	exe.domainSep = exe.computeDomainSeparator(ctfExchangeAddr)
	exe.domainSepNegRisk = exe.computeDomainSeparator(ctfNegRiskExchangeAddr)
	return exe, nil
}

// PlaceOrder signs and submits a FOK limit order at the opportunity price.
func (l *LiveExecutor) PlaceOrder(ctx context.Context, opp market.Opportunity) error {
	if opp.TokenID == "" {
		return fmt.Errorf("live: no token_id for %s side=%q", opp.ConditionID[:12], opp.Side)
	}

	makerAmt := int64(opp.SizeUSDC*1e6 + 0.5)
	takerAmt := int64(opp.Shares*1e6 + 0.5)

	tokenID := new(big.Int)
	if _, ok := tokenID.SetString(opp.TokenID, 10); !ok {
		return fmt.Errorf("live: invalid token_id %q for %s", opp.TokenID, opp.ConditionID[:12])
	}

	salt, err := generateSalt()
	if err != nil {
		return fmt.Errorf("live: generate salt: %w", err)
	}

	tsMs := time.Now().UnixMilli()

	sig, err := l.signOrder(salt, tokenID, makerAmt, takerAmt, 0, tsMs, opp.NegRisk)
	if err != nil {
		return fmt.Errorf("live: sign order: %w", err)
	}

	const bytes32ZeroHex = "0x0000000000000000000000000000000000000000000000000000000000000000"

	type orderJSON struct {
		Builder       string `json:"builder"`
		Expiration    string `json:"expiration"`
		Maker         string `json:"maker"`
		MakerAmount   string `json:"makerAmount"`
		Metadata      string `json:"metadata"`
		Salt          int64  `json:"salt"`
		Side          string `json:"side"`
		Signature     string `json:"signature"`
		SignatureType int    `json:"signatureType"`
		Signer        string `json:"signer"`
		TakerAmount   string `json:"takerAmount"`
		Timestamp     string `json:"timestamp"`
		TokenID       string `json:"tokenId"`
	}
	type reqBody struct {
		DeferExec bool      `json:"deferExec"`
		Order     orderJSON `json:"order"`
		OrderType string    `json:"orderType"`
		Owner     string    `json:"owner"`
	}

	body := reqBody{
		DeferExec: false,
		Order: orderJSON{
			Builder:       bytes32ZeroHex,
			Expiration:    "0",
			Maker:         l.proxyWallet, // deposit wallet is maker
			MakerAmount:   fmt.Sprintf("%d", makerAmt),
			Metadata:      bytes32ZeroHex,
			Salt:          salt.Int64(),
			Side:          "BUY",
			Signature:     sig,
			SignatureType: 3, // POLY_1271 — ERC-7739 TypedDataSign
			Signer:        l.proxyWallet, // deposit wallet is also signer for POLY_1271
			TakerAmount:   fmt.Sprintf("%d", takerAmt),
			Timestamp:     fmt.Sprintf("%d", tsMs),
			TokenID:       opp.TokenID,
		},
		OrderType: "FOK",
		Owner:     l.apiKey,
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("live: marshal order: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", clobBaseURL+"/order", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("live: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	l.setAuthHeaders(req, "POST", "/order", string(bodyBytes))

	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("live: POST /order: %w", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("live: CLOB rejected order (HTTP %d): %v", resp.StatusCode, result)
	}

	log.Printf("[LIVE] ✅ %-10s | %-50s | %s @ %.1f¢ | $%.2f | resp=%v",
		opp.Sport, truncate(opp.Market, 50), opp.Side, opp.Price*100, opp.SizeUSDC, result)

	if l.db != nil {
		buyFee := kelly.CalcBuyFee(opp.Shares, opp.Price, opp.Sport)
		if err := l.db.InsertLiveTrade(db.PaperTrade{
			ConditionID: opp.ConditionID,
			Market:      opp.Market,
			Slug:        opp.Slug,
			Sport:       opp.Sport,
			Side:        opp.Side,
			EntryPrice:  opp.Price,
			Shares:      opp.Shares,
			SizeUSDC:    opp.SizeUSDC,
			BuyFee:      buyFee,
		}); err != nil {
			log.Printf("[LIVE] warning: DB write failed for %s: %v", opp.ConditionID[:12], err)
		}
	}

	return nil
}

// VerifyCredentials calls GET /auth/api-keys to confirm the L2 credentials are valid.
func (l *LiveExecutor) VerifyCredentials(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", clobBaseURL+"/auth/api-keys", nil)
	if err != nil {
		return fmt.Errorf("verify creds: build request: %w", err)
	}
	l.setAuthHeaders(req, "GET", "/auth/api-keys", "")

	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("verify creds: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("verify creds: HTTP %d — check POLY_API_KEY / POLY_API_SECRET / POLY_API_PASSPHRASE", resp.StatusCode)
	}
	log.Printf("[live] credentials verified ✓ (address: %s)", l.address)
	return nil
}

// PlaceSellOrder places a FOK SELL order on the CLOB to exit a live position.
func (l *LiveExecutor) PlaceSellOrder(ctx context.Context, tokenID, side string, shares, stopPrice float64, negRisk bool) error {
	makerAmt := int64(shares*1e6 + 0.5)
	takerAmt := int64(shares*stopPrice*1e6 + 0.5)

	tokenIDBig := new(big.Int)
	if _, ok := tokenIDBig.SetString(tokenID, 10); !ok {
		return fmt.Errorf("live sell: invalid token_id %q", tokenID)
	}

	salt, err := generateSalt()
	if err != nil {
		return fmt.Errorf("live sell: generate salt: %w", err)
	}

	tsMs := time.Now().UnixMilli()

	sig, err := l.signOrder(salt, tokenIDBig, makerAmt, takerAmt, 1, tsMs, negRisk)
	if err != nil {
		return fmt.Errorf("live sell: sign order: %w", err)
	}

	const bytes32ZeroHex = "0x0000000000000000000000000000000000000000000000000000000000000000"

	type orderJSON struct {
		Builder       string `json:"builder"`
		Expiration    string `json:"expiration"`
		Maker         string `json:"maker"`
		MakerAmount   string `json:"makerAmount"`
		Metadata      string `json:"metadata"`
		Salt          int64  `json:"salt"`
		Side          string `json:"side"`
		Signature     string `json:"signature"`
		SignatureType int    `json:"signatureType"`
		Signer        string `json:"signer"`
		TakerAmount   string `json:"takerAmount"`
		Timestamp     string `json:"timestamp"`
		TokenID       string `json:"tokenId"`
	}
	type reqBody struct {
		DeferExec bool      `json:"deferExec"`
		Order     orderJSON `json:"order"`
		OrderType string    `json:"orderType"`
		Owner     string    `json:"owner"`
	}

	body := reqBody{
		DeferExec: false,
		Order: orderJSON{
			Builder:       bytes32ZeroHex,
			Expiration:    "0",
			Maker:         l.proxyWallet,
			MakerAmount:   fmt.Sprintf("%d", makerAmt),
			Metadata:      bytes32ZeroHex,
			Salt:          salt.Int64(),
			Side:          "SELL",
			Signature:     sig,
			SignatureType: 3, // POLY_1271
			Signer:        l.proxyWallet,
			TakerAmount:   fmt.Sprintf("%d", takerAmt),
			Timestamp:     fmt.Sprintf("%d", tsMs),
			TokenID:       tokenID,
		},
		OrderType: "FOK",
		Owner:     l.apiKey,
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("live sell: marshal order: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", clobBaseURL+"/order", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("live sell: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	l.setAuthHeaders(req, "POST", "/order", string(bodyBytes))

	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("live sell: POST /order: %w", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("live sell: CLOB rejected order (HTTP %d): %v", resp.StatusCode, result)
	}

	log.Printf("[LIVE] ⛔ SELL stop-loss | %s @ %.1f¢ | %.2f shares | resp=%v",
		side, stopPrice*100, shares, result)
	return nil
}

// ── EIP-712 / ERC-7739 signing ────────────────────────────────────────────────

// signOrder builds the ERC-7739 TypedDataSign digest, signs it, and returns
// the ERC-7739 wrapped signature (0x-prefixed hex).
//
// For POLY_1271 (signatureType=3), the signature format is:
//
//	[65-byte ECDSA sig] + [32-byte app domain sep] + [32-byte order hash] +
//	[ORDER_TYPE_STRING bytes] + [2-byte big-endian length of ORDER_TYPE_STRING]
func (l *LiveExecutor) signOrder(salt, tokenID *big.Int, makerAmt, takerAmt, side, tsMs int64, negRisk bool) (string, error) {
	// 1. Compute the Order struct hash (used in both TypedDataSign and the ERC-7739 append).
	orderHash := l.computeOrderHash(salt, tokenID, makerAmt, takerAmt, side, tsMs)

	// 2. Select exchange domain separator.
	domSep := l.domainSep
	if negRisk {
		domSep = l.domainSepNegRisk
	}

	// 3. Compute TypedDataSign struct hash (ERC-7739 envelope).
	typedDataSignHash := computeTypedDataSignHash(orderHash, l.proxyWallet)

	// 4. Final EIP-712 digest: "\x19\x01" || exchangeDomainSep || typedDataSignHash
	digest := keccak256(append(append([]byte{0x19, 0x01}, domSep...), typedDataSignHash...))

	// 5. ECDSA sign with EOA private key.
	privKey, err := gethcrypto.ToECDSA(l.privateKey)
	if err != nil {
		return "", err
	}
	ecdsaSig, err := gethcrypto.Sign(digest, privKey)
	if err != nil {
		return "", err
	}
	// go-ethereum returns v ∈ {0,1}; Ethereum wallets expect v ∈ {27,28}.
	if ecdsaSig[64] < 27 {
		ecdsaSig[64] += 27
	}

	// 6. ERC-7739 wrapping: append appDomainSep + orderHash + typeString + typeLen.
	contentsType := []byte(orderTypeSig)
	typeLen := []byte{byte(len(contentsType) >> 8), byte(len(contentsType) & 0xff)}

	wrapped := make([]byte, 0, 65+32+32+len(contentsType)+2)
	wrapped = append(wrapped, ecdsaSig...)    // 65 bytes: ECDSA sig
	wrapped = append(wrapped, domSep...)      // 32 bytes: app (exchange) domain separator
	wrapped = append(wrapped, orderHash...)   // 32 bytes: Order struct hash
	wrapped = append(wrapped, contentsType...) // ORDER_TYPE_STRING as bytes
	wrapped = append(wrapped, typeLen...)     // 2 bytes: big-endian length

	return "0x" + hex.EncodeToString(wrapped), nil
}

// computeOrderHash returns the EIP-712 V2 struct hash for an Order.
// For POLY_1271: maker = signer = deposit wallet (proxyWallet), signatureType = 3.
func (l *LiveExecutor) computeOrderHash(salt, tokenID *big.Int, makerAmt, takerAmt, side, tsMs int64) []byte {
	typeHash := keccak256([]byte(orderTypeSig))

	sigType3 := make([]byte, 32)
	sigType3[31] = 3 // signatureType = 3 (POLY_1271)

	sideBytes := make([]byte, 32)
	sideBytes[31] = byte(side)

	walletHex := strings.TrimPrefix(l.proxyWallet, "0x")

	return keccak256(concatBytes(
		typeHash,
		padBigInt(salt),
		padHexAddr(walletHex), // maker = deposit wallet
		padHexAddr(walletHex), // signer = deposit wallet (for POLY_1271)
		padBigInt(tokenID),
		padBigInt(big.NewInt(makerAmt)),
		padBigInt(big.NewInt(takerAmt)),
		sideBytes,
		sigType3,
		padBigInt(big.NewInt(tsMs)),
		bytes32Zero, // metadata = bytes32(0)
		bytes32Zero, // builder = bytes32(0)
	))
}

// computeTypedDataSignHash computes the ERC-7739 TypedDataSign struct hash.
// This wraps the Order inside a TypedDataSign envelope where the deposit wallet
// is the ERC-1271 verifying contract.
func computeTypedDataSignHash(orderHash []byte, depositWallet string) []byte {
	typeHash := keccak256([]byte(typedDataSignTypeSig))
	nameHash := keccak256([]byte("DepositWallet"))
	versionHash := keccak256([]byte("1"))
	chainID := padBigInt(big.NewInt(polygonChainID))
	walletAddr := padHexAddr(strings.TrimPrefix(depositWallet, "0x"))

	return keccak256(concatBytes(
		typeHash,
		orderHash,    // contents = Order struct hash
		nameHash,     // name = "DepositWallet"
		versionHash,  // version = "1"
		chainID,      // chainId = 137
		walletAddr,   // verifyingContract = deposit wallet
		bytes32Zero,  // salt = bytes32(0)
	))
}

// computeDomainSeparator pre-computes the EIP-712 domain separator.
func (l *LiveExecutor) computeDomainSeparator(exchangeAddr string) []byte {
	typeHash    := keccak256([]byte(domainTypeSig))
	nameHash    := keccak256([]byte("Polymarket CTF Exchange"))
	versionHash := keccak256([]byte(protocolVersion))
	chainID     := padBigInt(big.NewInt(polygonChainID))
	contract    := padHexAddr(exchangeAddr)

	return keccak256(concatBytes(typeHash, nameHash, versionHash, chainID, contract))
}

// ── L2 API auth headers ───────────────────────────────────────────────────────

// setAuthHeaders adds the five Polymarket L2 authentication headers to req.
// L2 auth is always identified by the EOA address (POLY_ADDRESS), not the wallet.
func (l *LiveExecutor) setAuthHeaders(req *http.Request, method, path, body string) {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, l.apiSecret)
	mac.Write([]byte(ts + method + path + body))
	sig := base64.URLEncoding.EncodeToString(mac.Sum(nil))

	req.Header.Set("POLY_ADDRESS",    l.address)
	req.Header.Set("POLY_API_KEY",    l.apiKey)
	req.Header.Set("POLY_SIGNATURE",  sig)
	req.Header.Set("POLY_TIMESTAMP",  ts)
	req.Header.Set("POLY_PASSPHRASE", l.apiPassphrase)
}

// ── crypto helpers ────────────────────────────────────────────────────────────

func keccak256(data []byte) []byte {
	return gethcrypto.Keccak256(data)
}

func padBigInt(n *big.Int) []byte {
	b := n.Bytes()
	if len(b) == 32 {
		return b
	}
	pad := make([]byte, 32-len(b))
	return append(pad, b...)
}

func padHexAddr(addr string) []byte {
	b, _ := hex.DecodeString(addr)
	pad := make([]byte, 32-len(b))
	return append(pad, b...)
}

func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// generateSalt returns a random salt capped to 53 bits so it survives lossless
// round-trip through JavaScript's IEEE-754 Number type on the CLOB wire.
func generateSalt() (*big.Int, error) {
	saltBytes := make([]byte, 8)
	if _, err := rand.Read(saltBytes); err != nil {
		return nil, err
	}
	salt := new(big.Int).SetBytes(saltBytes)
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 53), big.NewInt(1))
	salt.And(salt, mask)
	return salt, nil
}
