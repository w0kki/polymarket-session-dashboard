package executor

// LiveExecutor — places real orders on the Polymarket CLOB.
//
// Order flow:
//  1. Build order parameters (salt, amounts in micro-units)
//  2. Sign with EIP-712 using the EOA private key
//  3. POST signed order to clob.polymarket.com/order with L2 auth headers
//  4. Parse fill response and log the result
//
// EIP-712 domain: "ClobAuthDomain" v1, Polygon (chainId=137)
// Exchange:       CTF Exchange — 0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E

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
	clobBaseURL        = "https://clob.polymarket.com"
	ctfExchangeAddr    = "4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E" // no 0x prefix for abi encoding
	polygonChainID     = int64(137)

	// EIP-712 type strings — must match exactly what the contract expects.
	domainTypeSig = "EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"
	orderTypeSig  = "Order(uint256 salt,address maker,address signer,address taker,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint256 expiration,uint256 nonce,uint256 feeRateBps,uint8 side,uint8 signatureType)"
)

// LiveExecutor places real orders on the Polymarket CLOB via EIP-712 signed
// limit orders submitted as Fill-or-Kill (FOK) — they fill at the observed
// price or are cancelled immediately, preventing partial fills.
//
// Polymarket uses a proxy-wallet architecture:
//   - proxyWallet  — the Polymarket-managed account that holds USDC (maker)
//   - address      — the MetaMask EOA that signs orders (signer)
//   - signatureType 1 (Poly Proxy) tells the CTF Exchange contract to verify
//     the EOA signature against the proxy wallet's authorisation record.
type LiveExecutor struct {
	privateKey    []byte // 32-byte secp256k1 private key
	address       string // checksummed EOA address derived from private key (signer)
	proxyWallet   string // Polymarket proxy wallet address (maker / fund holder)
	apiKey        string
	apiSecret     []byte // base64-decoded HMAC signing key
	apiPassphrase string
	client        *http.Client
	domainSep     []byte // pre-computed EIP-712 domain separator
	db            *db.DB // shared SQLite DB — write trade record immediately after fill
}

// NewLive constructs a LiveExecutor from credentials and the shared database.
// proxyWallet is the Polymarket proxy wallet address (maker); it differs from
// the EOA address derived from privateKeyHex (signer). Both are required.
// Returns an error if any credential is missing or malformed.
func NewLive(privateKeyHex, apiKey, apiSecret, passphrase, proxyWallet string, database *db.DB) (*LiveExecutor, error) {
	if privateKeyHex == "" || apiKey == "" || apiSecret == "" || passphrase == "" {
		return nil, fmt.Errorf("live executor: POLY_PRIVATE_KEY, POLY_API_KEY, POLY_API_SECRET and POLY_API_PASSPHRASE must all be set")
	}
	if proxyWallet == "" {
		return nil, fmt.Errorf("live executor: POLY_PROXY_WALLET must be set (your Polymarket proxy wallet address, not your MetaMask EOA)")
	}

	// Parse private key.
	keyHex := strings.TrimPrefix(privateKeyHex, "0x")
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil || len(keyBytes) != 32 {
		return nil, fmt.Errorf("live executor: invalid POLY_PRIVATE_KEY (must be 32-byte hex): %w", err)
	}

	// Derive the wallet address from the private key.
	privKey, err := gethcrypto.ToECDSA(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("live executor: cannot parse private key: %w", err)
	}
	address := gethcrypto.PubkeyToAddress(privKey.PublicKey).Hex()

	// Decode the API secret — Polymarket emits base64url (with - and _).
	secretBytes, err := base64.URLEncoding.DecodeString(apiSecret)
	if err != nil {
		// Fall back to standard base64.
		secretBytes, err = base64.StdEncoding.DecodeString(apiSecret)
		if err != nil {
			return nil, fmt.Errorf("live executor: cannot decode POLY_API_SECRET (expected base64): %w", err)
		}
	}

	log.Printf("[live] executor ready — signer (EOA): %s", address)
	log.Printf("[live] executor ready — maker (proxy): %s", proxyWallet)

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
	exe.domainSep = exe.computeDomainSeparator()
	return exe, nil
}

// PlaceOrder signs and submits a FOK limit order at the opportunity price.
// The price acts as a worst-acceptable-price guarantee: if the best ask has
// moved above it the order is cancelled rather than filled at a worse price.
func (l *LiveExecutor) PlaceOrder(ctx context.Context, opp market.Opportunity) error {
	if opp.TokenID == "" {
		return fmt.Errorf("live: no token_id for %s side=%q — market may have changed since scan", opp.ConditionID[:12], opp.Side)
	}

	// ── Build order amounts ────────────────────────────────────────────────────
	// Polymarket uses 6-decimal micro-units for both USDC and CTF tokens.
	// makerAmount = USDC we spend; takerAmount = shares we receive.
	// Use math.Round to avoid truncation errors that shift the implied price
	// slightly and cause the CLOB to reject the order as malformed.
	makerAmt := int64(opp.SizeUSDC*1e6 + 0.5)
	takerAmt := int64(opp.Shares*1e6 + 0.5)

	tokenID := new(big.Int)
	if _, ok := tokenID.SetString(opp.TokenID, 10); !ok {
		return fmt.Errorf("live: invalid token_id %q for %s", opp.TokenID, opp.ConditionID[:12])
	}

	// Random 8-byte salt prevents replay attacks.
	saltBytes := make([]byte, 8)
	if _, err := rand.Read(saltBytes); err != nil {
		return fmt.Errorf("live: generate salt: %w", err)
	}
	salt := new(big.Int).SetBytes(saltBytes)

	// ── Sign (side=0 for BUY) ─────────────────────────────────────────────────
	sig, err := l.signOrder(salt, tokenID, makerAmt, takerAmt, 0)
	if err != nil {
		return fmt.Errorf("live: sign order: %w", err)
	}

	// ── Build request body ────────────────────────────────────────────────────
	type orderJSON struct {
		Salt          string `json:"salt"`
		Maker         string `json:"maker"`
		Signer        string `json:"signer"`
		Taker         string `json:"taker"`
		TokenID       string `json:"tokenId"`
		MakerAmount   string `json:"makerAmount"`
		TakerAmount   string `json:"takerAmount"`
		Expiration    string `json:"expiration"`
		Nonce         string `json:"nonce"`
		FeeRateBps    string `json:"feeRateBps"`
		Side          string `json:"side"`
		SignatureType int    `json:"signatureType"`
		Signature     string `json:"signature"`
	}
	type reqBody struct {
		Order     orderJSON `json:"order"`
		Owner     string    `json:"owner"`
		OrderType string    `json:"orderType"`
	}

	body := reqBody{
		Order: orderJSON{
			Salt:          salt.String(),
			Maker:         l.proxyWallet, // proxy wallet holds USDC (maker of funds)
			Signer:        l.address,     // EOA signs the order
			Taker:         "0x0000000000000000000000000000000000000000",
			TokenID:       opp.TokenID,
			MakerAmount:   fmt.Sprintf("%d", makerAmt),
			TakerAmount:   fmt.Sprintf("%d", takerAmt),
			Expiration:    "0",
			Nonce:         "0",
			FeeRateBps:    "0",
			Side:          "BUY",
			SignatureType: 1, // Poly Proxy — signer is EOA, maker is proxy wallet
			Signature:     sig,
		},
		Owner:     l.apiKey,
		OrderType: "FOK",
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("live: marshal order: %w", err)
	}

	// ── Authenticate + POST ───────────────────────────────────────────────────
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

	// ── Write to DB immediately ───────────────────────────────────────────────
	// This ensures dedup, stop-loss monitoring, and Kelly sizing all work
	// without waiting for the hourly sync.js run.
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

// VerifyCredentials calls GET /auth/api-keys to confirm the L2 credentials are
// valid before any real order is placed. Returns an error if the credentials
// are rejected; a 200 with the key list means the API key is registered and the
// HMAC secret + passphrase are correct.
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

// PlaceSellOrder places a FOK SELL order on the CLOB to exit a live position
// at the stop-loss price. Used exclusively by the live stop-loss handler.
// tokenID is the CLOB token_id for the outcome being sold (fetched dynamically).
// stopPrice is the minimum acceptable USDC per share (worst price we'll accept).
func (l *LiveExecutor) PlaceSellOrder(ctx context.Context, tokenID, side string, shares, stopPrice float64) error {
	// SELL: makerAmount = shares we're offering; takerAmount = USDC we want back.
	makerAmt := int64(shares*1e6 + 0.5)
	takerAmt := int64(shares*stopPrice*1e6 + 0.5)

	tokenIDBig := new(big.Int)
	if _, ok := tokenIDBig.SetString(tokenID, 10); !ok {
		return fmt.Errorf("live sell: invalid token_id %q", tokenID)
	}

	saltBytes := make([]byte, 8)
	if _, err := rand.Read(saltBytes); err != nil {
		return fmt.Errorf("live sell: generate salt: %w", err)
	}
	salt := new(big.Int).SetBytes(saltBytes)

	sig, err := l.signOrder(salt, tokenIDBig, makerAmt, takerAmt, 1) // side=1 for SELL
	if err != nil {
		return fmt.Errorf("live sell: sign order: %w", err)
	}

	type orderJSON struct {
		Salt          string `json:"salt"`
		Maker         string `json:"maker"`
		Signer        string `json:"signer"`
		Taker         string `json:"taker"`
		TokenID       string `json:"tokenId"`
		MakerAmount   string `json:"makerAmount"`
		TakerAmount   string `json:"takerAmount"`
		Expiration    string `json:"expiration"`
		Nonce         string `json:"nonce"`
		FeeRateBps    string `json:"feeRateBps"`
		Side          string `json:"side"`
		SignatureType int    `json:"signatureType"`
		Signature     string `json:"signature"`
	}
	type reqBody struct {
		Order     orderJSON `json:"order"`
		Owner     string    `json:"owner"`
		OrderType string    `json:"orderType"`
	}

	body := reqBody{
		Order: orderJSON{
			Salt:          salt.String(),
			Maker:         l.proxyWallet, // proxy wallet holds the tokens being sold
			Signer:        l.address,     // EOA signs the order
			Taker:         "0x0000000000000000000000000000000000000000",
			TokenID:       tokenID,
			MakerAmount:   fmt.Sprintf("%d", makerAmt),
			TakerAmount:   fmt.Sprintf("%d", takerAmt),
			Expiration:    "0",
			Nonce:         "0",
			FeeRateBps:    "0",
			Side:          "SELL",
			SignatureType: 1, // Poly Proxy
			Signature:     sig,
		},
		Owner:     l.apiKey,
		OrderType: "FOK",
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

// ── EIP-712 signing ───────────────────────────────────────────────────────────

// signOrder builds the EIP-712 digest and returns a 0x-prefixed hex signature.
// side: 0 = BUY, 1 = SELL (matches the CLOB's uint8 side field in the order struct).
func (l *LiveExecutor) signOrder(salt, tokenID *big.Int, makerAmt, takerAmt int64, side int64) (string, error) {
	orderHash := l.computeOrderHash(salt, tokenID, makerAmt, takerAmt, side)

	// "\x19\x01" ‖ domainSeparator ‖ orderHash
	digest := keccak256(append(append([]byte{0x19, 0x01}, l.domainSep...), orderHash...))

	privKey, err := gethcrypto.ToECDSA(l.privateKey)
	if err != nil {
		return "", err
	}

	sig, err := gethcrypto.Sign(digest, privKey)
	if err != nil {
		return "", err
	}

	// go-ethereum returns v ∈ {0,1}; Ethereum wallets expect v ∈ {27,28}.
	if sig[64] < 27 {
		sig[64] += 27
	}

	return "0x" + hex.EncodeToString(sig), nil
}

// computeDomainSeparator pre-computes the EIP-712 domain separator once at
// startup.  Domain: name="ClobAuthDomain", version="1", chainId=137,
// verifyingContract=CTF Exchange.
func (l *LiveExecutor) computeDomainSeparator() []byte {
	typeHash  := keccak256([]byte(domainTypeSig))
	nameHash  := keccak256([]byte("ClobAuthDomain"))
	versionHash := keccak256([]byte("1"))
	chainID   := padBigInt(big.NewInt(polygonChainID))
	contract  := padHexAddr(ctfExchangeAddr)

	return keccak256(concatBytes(typeHash, nameHash, versionHash, chainID, contract))
}

// computeOrderHash returns the EIP-712 struct hash for an Order.
// side: 0 = BUY, 1 = SELL.
// maker = proxyWallet (funds holder); signer = EOA; signatureType = 1 (Poly Proxy).
func (l *LiveExecutor) computeOrderHash(salt, tokenID *big.Int, makerAmt, takerAmt, side int64) []byte {
	typeHash := keccak256([]byte(orderTypeSig))
	zero32   := make([]byte, 32)

	sigType1 := make([]byte, 32)
	sigType1[31] = 1 // signatureType = 1 (Poly Proxy)

	return keccak256(concatBytes(
		typeHash,
		padBigInt(salt),                                                  // salt
		padHexAddr(strings.TrimPrefix(l.proxyWallet, "0x")),             // maker = proxy wallet
		padHexAddr(strings.TrimPrefix(l.address, "0x")),                  // signer = EOA
		zero32,                                                           // taker = address(0)
		padBigInt(tokenID),                                               // tokenId
		padBigInt(big.NewInt(makerAmt)),                                  // makerAmount
		padBigInt(big.NewInt(takerAmt)),                                  // takerAmount
		zero32,                                                           // expiration = 0
		zero32,                                                           // nonce = 0
		zero32,                                                           // feeRateBps = 0
		padBigInt(big.NewInt(side)),                                      // side: 0=BUY, 1=SELL
		sigType1,                                                         // signatureType = 1 (Poly Proxy)
	))
}

// ── L2 API auth headers ───────────────────────────────────────────────────────

// setAuthHeaders adds the five Polymarket L2 authentication headers to req.
// Signature = base64url( HMAC-SHA256(timestamp+method+path+body, apiSecret) ).
// Must use URL-safe base64 output to match the py-clob-client reference implementation.
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

// padBigInt encodes n as a big-endian 32-byte slice (ABI uint256).
func padBigInt(n *big.Int) []byte {
	b := n.Bytes()
	if len(b) == 32 {
		return b
	}
	pad := make([]byte, 32-len(b))
	return append(pad, b...)
}

// padHexAddr encodes a 20-byte Ethereum address as a 32-byte ABI word.
// addr must be a 40-char hex string (no 0x prefix).
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
